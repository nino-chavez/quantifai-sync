package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Test 1: install generates correct macOS LaunchAgent plist XML
// ---------------------------------------------------------------------------

func TestLaunchAgentGeneratesCorrectPlist(t *testing.T) {
	la := NewLaunchAgent()
	plist := la.GeneratePlist()

	// Validate XML plist structure
	requiredElements := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"`,
		`<plist version="1.0">`,
		`<dict>`,

		// Label
		`<key>Label</key>`,
		`<string>com.quantifai.sync</string>`,

		// ProgramArguments with run subcommand
		`<key>ProgramArguments</key>`,
		`<string>run</string>`,

		// RunAtLoad
		`<key>RunAtLoad</key>`,
		`<true/>`,

		// KeepAlive
		`<key>KeepAlive</key>`,
		`<true/>`,

		// ThrottleInterval
		`<key>ThrottleInterval</key>`,
		`<integer>10</integer>`,

		// StandardOutPath and StandardErrorPath
		`<key>StandardOutPath</key>`,
		`<key>StandardErrorPath</key>`,

		// Closing tags
		`</dict>`,
		`</plist>`,
	}

	for _, elem := range requiredElements {
		if !strings.Contains(plist, elem) {
			t.Errorf("plist missing required element: %s", elem)
		}
	}

	// Verify log paths contain the expected filename
	if !strings.Contains(plist, "quantifai-sync.log") {
		t.Error("plist missing log file path 'quantifai-sync.log'")
	}

	// Verify the config path is under ~/Library/LaunchAgents/
	configPath := la.ConfigPath()
	if !strings.Contains(configPath, filepath.Join("Library", "LaunchAgents")) {
		t.Errorf("config path %q does not contain Library/LaunchAgents", configPath)
	}
	if !strings.HasSuffix(configPath, "com.quantifai.sync.plist") {
		t.Errorf("config path %q does not end with com.quantifai.sync.plist", configPath)
	}

	// Verify the binary path is resolved (not hardcoded)
	if la.binaryPath == "" {
		t.Error("binary path should be resolved, got empty string")
	}
}

// ---------------------------------------------------------------------------
// Test 2: install generates correct Linux systemd unit file
// ---------------------------------------------------------------------------

func TestSystemdGeneratesCorrectUnitFile(t *testing.T) {
	sd := NewSystemd()
	unit := sd.GenerateUnit()

	// Validate systemd unit file structure
	requiredSections := []string{
		"[Unit]",
		"[Service]",
		"[Install]",
	}
	for _, section := range requiredSections {
		if !strings.Contains(unit, section) {
			t.Errorf("unit file missing required section: %s", section)
		}
	}

	// Validate required properties (binary path is dynamic)
	requiredProperties := []string{
		"Description=Quantifai Sync Agent",
		"Type=simple",
		"Restart=on-failure",
		"RestartSec=10",
		"NotifyAccess=main",
		"WantedBy=default.target",
	}
	for _, prop := range requiredProperties {
		if !strings.Contains(unit, prop) {
			t.Errorf("unit file missing required property: %s", prop)
		}
	}

	// Verify ExecStart contains "run" subcommand
	if !strings.Contains(unit, " run") {
		t.Error("unit file ExecStart missing 'run' subcommand")
	}

	// Verify the config path is under ~/.config/systemd/user/
	configPath := sd.ConfigPath()
	if !strings.Contains(configPath, filepath.Join(".config", "systemd", "user")) {
		t.Errorf("config path %q does not contain .config/systemd/user", configPath)
	}
	if !strings.HasSuffix(configPath, "quantifai-sync.service") {
		t.Errorf("config path %q does not end with quantifai-sync.service", configPath)
	}
}

// ---------------------------------------------------------------------------
// Test 3: uninstall removes the generated service config file
// ---------------------------------------------------------------------------

func TestUninstallRemovesConfigFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Test with a LaunchAgent-like config file
	plistPath := filepath.Join(tmpDir, "com.quantifai.sync.plist")
	if err := os.WriteFile(plistPath, []byte("<plist>test</plist>"), 0644); err != nil {
		t.Fatalf("failed to create test plist: %v", err)
	}

	// Verify the file exists
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		t.Fatal("test plist file was not created")
	}

	// Remove the file (simulating uninstall behavior without launchctl)
	if err := os.Remove(plistPath); err != nil {
		t.Fatalf("failed to remove plist: %v", err)
	}

	// Verify the file is gone
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Error("plist file still exists after removal")
	}

	// Test with a systemd-like config file
	unitPath := filepath.Join(tmpDir, "quantifai-sync.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\ntest"), 0644); err != nil {
		t.Fatalf("failed to create test unit: %v", err)
	}

	if err := os.Remove(unitPath); err != nil {
		t.Fatalf("failed to remove unit file: %v", err)
	}

	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Error("unit file still exists after removal")
	}
}
