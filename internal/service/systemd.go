package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// systemdUnitName is the service name for the systemd user unit.
	systemdUnitName = "quantifai-sync.service"
	// legacySystemdUnitName is the old unit name used by ai-ops-shipper.
	legacySystemdUnitName = "ai-ops-shipper.service"
)

// unitTemplate is the systemd user unit file content.  Properties match
// the spec: Type=simple, Restart=on-failure, RestartSec=10,
// WantedBy=default.target.  NotifyAccess=main lets the agent set the
// status line `systemctl status` shows (see cmd/notify.go); without it
// systemd ignores every status message from a Type=simple service.  The ExecStart binary path (%s) is resolved
// dynamically at install time via os.Executable().
const unitTemplate = `[Unit]
Description=Quantifai Sync Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`

// Systemd implements the Installer interface for Linux.  It generates a
// systemd user unit at ~/.config/systemd/user/quantifai-sync.service and
// manages it via systemctl --user enable/disable --now.
type Systemd struct {
	unitPath   string
	binaryPath string
}

// NewSystemd creates a Systemd installer with paths derived from the
// current user's home directory.  The binary path is resolved via
// os.Executable() so the unit file always points to the actual install location.
func NewSystemd() *Systemd {
	home, _ := os.UserHomeDir()
	binPath, err := os.Executable()
	if err != nil {
		binPath = "/usr/local/bin/quantifai-sync"
	}
	return &Systemd{
		unitPath:   filepath.Join(home, ".config", "systemd", "user", systemdUnitName),
		binaryPath: binPath,
	}
}

// ConfigPath returns the unit file path.
func (s *Systemd) ConfigPath() string {
	return s.unitPath
}

// GenerateUnit returns the systemd unit file content with binary path resolved.
func (s *Systemd) GenerateUnit() string {
	return fmt.Sprintf(unitTemplate, s.binaryPath)
}

// Install writes the unit file, reloads systemd, and enables/starts the service.
func (s *Systemd) Install() error {
	// Ensure the systemd user unit directory exists
	dir := filepath.Dir(s.unitPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("service: create directory %s: %w", dir, err)
	}

	unitContent := s.GenerateUnit()
	if err := os.WriteFile(s.unitPath, []byte(unitContent), 0644); err != nil {
		return fmt.Errorf("service: write unit %s: %w", s.unitPath, err)
	}

	// Reload systemd to pick up the new unit file
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("service: systemctl daemon-reload: %s: %w", string(out), err)
	}

	// Enable and start the service
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnitName).CombinedOutput(); err != nil {
		return fmt.Errorf("service: systemctl enable --now: %s: %w", string(out), err)
	}

	return nil
}

// Uninstall stops/disables the service and removes the unit file.
func (s *Systemd) Uninstall() error {
	// Disable and stop the service (best-effort)
	exec.Command("systemctl", "--user", "disable", "--now", systemdUnitName).CombinedOutput()

	if err := os.Remove(s.unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove unit %s: %w", s.unitPath, err)
	}

	// Reload systemd to clean up
	exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()

	return nil
}

// MigrateFromOld detects the old ai-ops-shipper.service systemd unit,
// disables/stops it, removes the unit file, and reloads systemd.
// Returns nil if no old unit was found.
func (s *Systemd) MigrateFromOld() error {
	home, _ := os.UserHomeDir()
	oldUnit := filepath.Join(home, ".config", "systemd", "user", legacySystemdUnitName)

	if _, err := os.Stat(oldUnit); os.IsNotExist(err) {
		return nil // nothing to migrate
	}

	// Disable and stop the old service (best-effort)
	exec.Command("systemctl", "--user", "disable", "--now", legacySystemdUnitName).CombinedOutput()

	// Remove the old unit file
	if err := os.Remove(oldUnit); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove legacy unit %s: %w", oldUnit, err)
	}

	// Reload systemd to clean up
	exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()

	return nil
}
