package main

// Pipeline behavior (collect -> send -> commit offsets) is covered in
// cmd/cycle_test.go and internal/ingest; these tests cover state and config.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/quantifai/sync/internal/config"
	"github.com/quantifai/sync/internal/state"
)

// ---------------------------------------------------------------------------
// Test 6: State pruning -- stale entries for non-existent files are removed
// ---------------------------------------------------------------------------

func TestIntegrationStatePruning(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")

	// Create two real files
	realFile1 := filepath.Join(tmpDir, "real1.jsonl")
	realFile2 := filepath.Join(tmpDir, "real2.jsonl")
	os.WriteFile(realFile1, []byte("data\n"), 0644)
	os.WriteFile(realFile2, []byte("data\n"), 0644)

	// Create a state manager with entries for real and stale files
	mgr, err := state.NewManager(statePath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.Set(realFile1, state.FileState{ByteOffset: 100, Mtime: 1740825600.0})
	mgr.Set(realFile2, state.FileState{ByteOffset: 200, Mtime: 1740825700.0})
	mgr.Set("/nonexistent/stale1.jsonl", state.FileState{ByteOffset: 300, Mtime: 1740825800.0})
	mgr.Set("/nonexistent/stale2.jsonl", state.FileState{ByteOffset: 400, Mtime: 1740825900.0})
	mgr.Set("/also/missing/stale3.jsonl", state.FileState{ByteOffset: 500, Mtime: 1740826000.0})

	if err := mgr.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload state (simulating a restart)
	mgr2, err := state.NewManager(statePath)
	if err != nil {
		t.Fatalf("NewManager (reload): %v", err)
	}

	// Verify all 5 entries loaded
	if mgr2.TrackedFiles() != 5 {
		t.Fatalf("expected 5 tracked files after load, got %d", mgr2.TrackedFiles())
	}

	// Prune stale entries
	pruned := mgr2.Prune()
	if pruned != 3 {
		t.Errorf("expected 3 pruned entries, got %d", pruned)
	}

	if mgr2.TrackedFiles() != 2 {
		t.Errorf("expected 2 remaining tracked files, got %d", mgr2.TrackedFiles())
	}

	// Real files should still have their offsets
	fs1 := mgr2.Get(realFile1)
	if fs1.ByteOffset != 100 {
		t.Errorf("realFile1 offset: got %d, want 100", fs1.ByteOffset)
	}
	fs2 := mgr2.Get(realFile2)
	if fs2.ByteOffset != 200 {
		t.Errorf("realFile2 offset: got %d, want 200", fs2.ByteOffset)
	}

	// Stale entries should be gone
	fs3 := mgr2.Get("/nonexistent/stale1.jsonl")
	if fs3.ByteOffset != 0 {
		t.Errorf("stale entry should be gone, got offset %d", fs3.ByteOffset)
	}
}

// ---------------------------------------------------------------------------
// Test 7: Config layering integration -- env > user > system > defaults
// ---------------------------------------------------------------------------

func TestIntegrationConfigLayering(t *testing.T) {
	tmpDir := t.TempDir()

	systemPath := filepath.Join(tmpDir, "system.toml")
	userPath := filepath.Join(tmpDir, "user.toml")

	// System config: provides api_url, batch_size, log_level
	os.WriteFile(systemPath, []byte(`
api_url = "https://system.example.com"
batch_size = 3000
log_level = "debug"
flush_interval = 30
health_port = 19000
`), 0600)

	// User config: overrides api_url and batch_size from system, adds api_key
	os.WriteFile(userPath, []byte(`
api_url = "https://user.example.com"
batch_size = 4000
api_key = "user-file-key"
`), 0600)

	// Env vars: override api_url from both files
	// Clear both new and legacy env vars to avoid interference
	for _, prefix := range []string{"QUANTIFAI_", "AI_OPS_"} {
		for _, suffix := range []string{"API_URL", "API_KEY", "BATCH_SIZE", "LOG_LEVEL",
			"FLUSH_INTERVAL", "HEALTH_PORT", "SYNC_ENABLED", "AUTO_UPDATE",
			"UPDATE_CHANNEL", "WATCH_DIR", "STATE_FILE", "LOG_FILE"} {
			t.Setenv(prefix+suffix, "")
		}
	}
	t.Setenv("QUANTIFAI_API_URL", "https://env.example.com")

	cfg, err := config.Load(userPath, systemPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Env wins for api_url
	if cfg.APIURL != "https://env.example.com" {
		t.Errorf("APIURL: got %q, want env value", cfg.APIURL)
	}

	// User file wins for batch_size (no env set)
	if cfg.BatchSize != 4000 {
		t.Errorf("BatchSize: got %d, want 4000 (user file)", cfg.BatchSize)
	}

	// System file wins for log_level (user file did not set it, no env set)
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want debug (system file)", cfg.LogLevel)
	}

	// System file wins for flush_interval (user file did not set it)
	if cfg.FlushInterval != 30 {
		t.Errorf("FlushInterval: got %d, want 30 (system file)", cfg.FlushInterval)
	}

	// System file wins for health_port (user file did not set it)
	if cfg.HealthPort != 19000 {
		t.Errorf("HealthPort: got %d, want 19000 (system file)", cfg.HealthPort)
	}

	// User file wins for api_key
	if cfg.APIKey != "user-file-key" {
		t.Errorf("APIKey: got %q, want user-file-key", cfg.APIKey)
	}

	// Defaults win for auto_update (not set anywhere)
	if cfg.AutoUpdate {
		t.Error("AutoUpdate: got true, want false (default)")
	}

	// Defaults win for update_channel (not set anywhere)
	if cfg.UpdateChannel != "stable" {
		t.Errorf("UpdateChannel: got %q, want stable (default)", cfg.UpdateChannel)
	}

	// Verify validation passes
	if err := config.Validate(cfg); err != nil {
		t.Errorf("Validate should pass: %v", err)
	}
}
