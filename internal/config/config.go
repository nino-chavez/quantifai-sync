// Package config provides layered TOML configuration loading with
// environment variable overrides.  Resolution precedence (highest to lowest):
// environment variables > user config > system config > defaults.
//
// TOML key names and env var names are identical to src/config.py for
// contributor familiarity.  New QUANTIFAI_* env vars take precedence
// over legacy AI_OPS_* names for backwards compatibility.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all shipper configuration values.  Field names use Go
// conventions; the TOML keys and env var names they correspond to are
// documented in the spec's configuration table.
type Config struct {
	APIURL        string // TOML: api_url       | Env: QUANTIFAI_API_URL (legacy: AI_OPS_API_URL)
	APIKey        string // TOML: api_key       | Env: QUANTIFAI_API_KEY (legacy: AI_OPS_API_KEY)
	SyncEnabled   bool   // TOML: sync_enabled  | Env: QUANTIFAI_SYNC_ENABLED (legacy: AI_OPS_SYNC_ENABLED)
	WatchDir      string // TOML: watch_dir     | Env: QUANTIFAI_WATCH_DIR (legacy: AI_OPS_WATCH_DIR)
	StateFile     string // TOML: state_file    | Env: QUANTIFAI_STATE_FILE (legacy: AI_OPS_STATE_FILE)
	BatchSize     int    // TOML: batch_size    | Env: QUANTIFAI_BATCH_SIZE (legacy: AI_OPS_BATCH_SIZE)
	FlushInterval int    // TOML: flush_interval| Env: QUANTIFAI_FLUSH_INTERVAL (legacy: AI_OPS_FLUSH_INTERVAL)
	HealthPort    int    // TOML: health_port   | Env: QUANTIFAI_HEALTH_PORT (legacy: AI_OPS_HEALTH_PORT)
	LogLevel      string // TOML: log_level     | Env: QUANTIFAI_LOG_LEVEL (legacy: AI_OPS_LOG_LEVEL)
	LogFile       string // TOML: log_file      | Env: QUANTIFAI_LOG_FILE (legacy: AI_OPS_LOG_FILE)
	AutoUpdate          bool   // TOML: auto_update          | Env: QUANTIFAI_AUTO_UPDATE (legacy: AI_OPS_AUTO_UPDATE)
	UpdateChannel       string // TOML: update_channel       | Env: QUANTIFAI_UPDATE_CHANNEL (legacy: AI_OPS_UPDATE_CHANNEL)
	UpdateRepo          string // TOML: update_repo          | Env: QUANTIFAI_UPDATE_REPO
	UpdateCheckInterval string // TOML: update_check_interval| Env: QUANTIFAI_UPDATE_CHECK_INTERVAL (default "24h")
	GitEnabled          bool     // TOML: git_enabled          | Env: QUANTIFAI_GIT_ENABLED (default true)
	GitRepos            []string // TOML: git_repos            | NOT from env (complex type)
	GitProcessScan      bool     // TOML: git_process_scan     | Env: QUANTIFAI_GIT_PROCESS_SCAN (default true)
	KnowledgeTier3      bool     // TOML: knowledge_tier3      | Env: QUANTIFAI_KNOWLEDGE_TIER3 (default false, opt-in)
	IntentTagEnabled    bool     // TOML: intent_tag_enabled   | Env: QUANTIFAI_INTENT_TAG (default false, opt-in)
}

// DefaultUserConfigPath returns the config file path, preferring
// ~/.config/quantifai/config.toml and falling back to the legacy
// ~/.config/ai-ops-analytics/config.toml if the new path doesn't exist.
func DefaultUserConfigPath() string {
	home, _ := os.UserHomeDir()
	newPath := filepath.Join(home, ".config", "quantifai", "config.toml")
	if _, err := os.Stat(newPath); err == nil {
		return newPath
	}
	legacyPath := filepath.Join(home, ".config", "ai-ops-analytics", "config.toml")
	if _, err := os.Stat(legacyPath); err == nil {
		return legacyPath
	}
	return newPath // default to new path even if it doesn't exist yet
}

// DefaultSystemConfigPath is the system-level config managed by MDM.
// Falls back to legacy /etc/ai-ops/config.toml if the new path doesn't exist.
func DefaultSystemConfigPath() string {
	newPath := "/etc/quantifai/config.toml"
	if _, err := os.Stat(newPath); err == nil {
		return newPath
	}
	legacyPath := "/etc/ai-ops/config.toml"
	if _, err := os.Stat(legacyPath); err == nil {
		return legacyPath
	}
	return newPath
}

// DefaultAPIURL is the canonical server host. www.quantifai.app answers with
// a cross-host 308 to this apex, and Go drops the Authorization header on a
// cross-host redirect, so the www form fails as an unauthenticated request.
const DefaultAPIURL = "https://quantifai.app"

// defaults returns a Config populated with all default values from the spec.
func defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		APIURL:        DefaultAPIURL,
		SyncEnabled:   true,
		WatchDir:      filepath.Join(home, ".claude", "projects"),
		StateFile:     defaultStateFile(home),
		BatchSize:     5000,
		FlushInterval: 60,
		HealthPort:    19876,
		LogLevel:      "info",
		AutoUpdate:          false,
		UpdateChannel:       "stable",
		UpdateRepo:          "quantifai-app/sync",
		UpdateCheckInterval: "24h",
		GitEnabled:          true,
		GitProcessScan:      true,
		KnowledgeTier3:      false, // opt-in: surface pattern recipes in CLI
		IntentTagEnabled:    false, // opt-in: extract intent tags from first prompt for pattern detection
	}
}

// defaultStateFile returns the state file path, preferring the new location
// but falling back to the legacy one if it already contains data.
func defaultStateFile(home string) string {
	newPath := filepath.Join(home, ".config", "quantifai", "sync-state.json")
	legacyPath := filepath.Join(home, ".config", "ai-ops-analytics", "shipper-state.json")
	// If legacy state exists and new state doesn't, use legacy to avoid re-processing
	if _, err := os.Stat(legacyPath); err == nil {
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			return legacyPath
		}
	}
	return newPath
}

// Load builds a Config by layering system config, user config, and
// environment variables on top of defaults.  Callers may override the
// user/system paths for testing; pass empty strings to use the defaults.
func Load(userPath, systemPath string) (Config, error) {
	if userPath == "" {
		userPath = DefaultUserConfigPath()
	}
	if systemPath == "" {
		systemPath = DefaultSystemConfigPath()
	}

	cfg := defaults()

	// Layer 1 (lowest file layer): system config
	if sysValues, err := parseSimpleTOML(systemPath); err == nil {
		applyTOMLValues(&cfg, sysValues)
	}

	// Layer 2: user config (overrides system)
	if usrValues, err := parseSimpleTOML(userPath); err == nil {
		applyTOMLValues(&cfg, usrValues)
		warnWorldReadable(userPath, usrValues)
	}

	// Layer 3 (highest): environment variables
	applyEnvVars(&cfg)

	return cfg, nil
}

// Validate checks that required fields are present and returns a
// descriptive error when they are not.
func Validate(cfg Config) error {
	if cfg.APIURL == "" {
		return fmt.Errorf("config: api_url is required (set QUANTIFAI_API_URL or add api_url to config.toml)")
	}
	return nil
}

// --- TOML parsing (minimal, stdlib-only) ---

// parseSimpleTOML reads a flat key = value TOML file.  It intentionally
// mirrors the simple parser in src/config.py to keep the shipper dependency-free.
// It supports strings (quoted), booleans, and integers.
func parseSimpleTOML(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])

		// Strip inline comments on unquoted values
		if !strings.HasPrefix(value, "\"") && strings.Contains(value, "#") {
			value = strings.TrimSpace(value[:strings.Index(value, "#")])
		}

		// Remove surrounding quotes
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}

		result[key] = value
	}
	return result, nil
}

// applyTOMLValues merges parsed TOML key-value pairs into the Config struct.
func applyTOMLValues(cfg *Config, values map[string]string) {
	if v, ok := values["api_url"]; ok && v != "" {
		cfg.APIURL = v
	}
	if v, ok := values["api_key"]; ok && v != "" {
		cfg.APIKey = v
	}
	if v, ok := values["sync_enabled"]; ok {
		cfg.SyncEnabled = parseBool(v)
	}
	if v, ok := values["watch_dir"]; ok && v != "" {
		cfg.WatchDir = expandHome(v)
	}
	if v, ok := values["state_file"]; ok && v != "" {
		cfg.StateFile = expandHome(v)
	}
	if v, ok := values["batch_size"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BatchSize = n
		}
	}
	if v, ok := values["flush_interval"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.FlushInterval = n
		}
	}
	if v, ok := values["health_port"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.HealthPort = n
		}
	}
	if v, ok := values["log_level"]; ok && v != "" {
		cfg.LogLevel = v
	}
	if v, ok := values["log_file"]; ok && v != "" {
		cfg.LogFile = expandHome(v)
	}
	if v, ok := values["auto_update"]; ok {
		cfg.AutoUpdate = parseBool(v)
	}
	if v, ok := values["update_channel"]; ok && v != "" {
		cfg.UpdateChannel = v
	}
	if v, ok := values["update_repo"]; ok && v != "" {
		cfg.UpdateRepo = v
	}
	if v, ok := values["update_check_interval"]; ok && v != "" {
		cfg.UpdateCheckInterval = v
	}
	if v, ok := values["git_enabled"]; ok {
		cfg.GitEnabled = parseBool(v)
	}
	if v, ok := values["git_repos"]; ok && v != "" {
		// Simple comma-separated list for flat TOML parsing
		repos := strings.Split(v, ",")
		cfg.GitRepos = make([]string, 0, len(repos))
		for _, r := range repos {
			r = strings.TrimSpace(r)
			if r != "" {
				cfg.GitRepos = append(cfg.GitRepos, expandHome(r))
			}
		}
	}
	if v, ok := values["git_process_scan"]; ok {
		cfg.GitProcessScan = parseBool(v)
	}
	if v, ok := values["knowledge_tier3"]; ok {
		cfg.KnowledgeTier3 = parseBool(v)
	}
	if v, ok := values["intent_tag_enabled"]; ok {
		cfg.IntentTagEnabled = parseBool(v)
	}
}

// envOrLegacy returns the value of the new env var if set, otherwise
// the legacy env var.  Returns empty string if neither is set.
func envOrLegacy(newKey, legacyKey string) string {
	if v := os.Getenv(newKey); v != "" {
		return v
	}
	return os.Getenv(legacyKey)
}

// applyEnvVars overlays environment variable values onto the Config.
// This is the highest-precedence layer.  QUANTIFAI_* env vars take
// precedence over legacy AI_OPS_* names.
func applyEnvVars(cfg *Config) {
	if v := envOrLegacy("QUANTIFAI_API_URL", "AI_OPS_API_URL"); v != "" {
		cfg.APIURL = v
	}
	if v := envOrLegacy("QUANTIFAI_API_KEY", "AI_OPS_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := envOrLegacy("QUANTIFAI_SYNC_ENABLED", "AI_OPS_SYNC_ENABLED"); v != "" {
		cfg.SyncEnabled = parseBool(v)
	}
	if v := envOrLegacy("QUANTIFAI_WATCH_DIR", "AI_OPS_WATCH_DIR"); v != "" {
		cfg.WatchDir = v
	}
	if v := envOrLegacy("QUANTIFAI_STATE_FILE", "AI_OPS_STATE_FILE"); v != "" {
		cfg.StateFile = v
	}
	if v := envOrLegacy("QUANTIFAI_BATCH_SIZE", "AI_OPS_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BatchSize = n
		}
	}
	if v := envOrLegacy("QUANTIFAI_FLUSH_INTERVAL", "AI_OPS_FLUSH_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.FlushInterval = n
		}
	}
	if v := envOrLegacy("QUANTIFAI_HEALTH_PORT", "AI_OPS_HEALTH_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.HealthPort = n
		}
	}
	if v := envOrLegacy("QUANTIFAI_LOG_LEVEL", "AI_OPS_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := envOrLegacy("QUANTIFAI_LOG_FILE", "AI_OPS_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := envOrLegacy("QUANTIFAI_AUTO_UPDATE", "AI_OPS_AUTO_UPDATE"); v != "" {
		cfg.AutoUpdate = parseBool(v)
	}
	if v := envOrLegacy("QUANTIFAI_UPDATE_CHANNEL", "AI_OPS_UPDATE_CHANNEL"); v != "" {
		cfg.UpdateChannel = v
	}
	if v := os.Getenv("QUANTIFAI_UPDATE_REPO"); v != "" {
		cfg.UpdateRepo = v
	}
	if v := os.Getenv("QUANTIFAI_UPDATE_CHECK_INTERVAL"); v != "" {
		cfg.UpdateCheckInterval = v
	}
	if v := os.Getenv("QUANTIFAI_GIT_ENABLED"); v != "" {
		cfg.GitEnabled = parseBool(v)
	}
	// GitRepos is not settable via env var (complex type)
	if v := os.Getenv("QUANTIFAI_GIT_PROCESS_SCAN"); v != "" {
		cfg.GitProcessScan = parseBool(v)
	}
	if v := os.Getenv("QUANTIFAI_KNOWLEDGE_TIER3"); v != "" {
		cfg.KnowledgeTier3 = parseBool(v)
	}
	if v := os.Getenv("QUANTIFAI_INTENT_TAG"); v != "" {
		cfg.IntentTagEnabled = parseBool(v)
	}
}

// parseBool interprets "true", "1", "yes" (case-insensitive) as true;
// everything else is false.  This matches the Python config loader behavior.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}

// warnWorldReadable prints a warning to stderr if a config file containing
// an api_key has overly permissive file permissions.
func warnWorldReadable(path string, values map[string]string) {
	if _, hasKey := values["api_key"]; !hasKey {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	mode := info.Mode().Perm()
	// Warn if group or other has read permission
	if mode&0o044 != 0 {
		fmt.Fprintf(os.Stderr, "WARNING: config file %s containing api_key is world-readable (mode %04o); consider chmod 600\n", path, mode)
	}
}
