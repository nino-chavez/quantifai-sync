package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// plistLabel is the reverse-DNS label for the daemon LaunchAgent.
	plistLabel = "com.quantifai.sync"
	// trayPlistLabel is the reverse-DNS label for the tray GUI LaunchAgent.
	trayPlistLabel = "com.quantifai.sync-tray"
	// legacyPlistLabel is the old label used by ai-ops-shipper.
	legacyPlistLabel = "com.ai-ops.shipper"
)

// plistTemplate is the macOS LaunchAgent plist XML template.  The
// properties match the spec: RunAtLoad=true, KeepAlive=true,
// ThrottleInterval=10, StandardOutPath/StandardErrorPath pointing to
// ~/Library/Logs/quantifai-sync.log, and ProgramArguments launching
// the binary with the "run" subcommand.  The binary path (%s) is
// resolved dynamically at install time via os.Executable().
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.quantifai.sync</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`

// trayPlistTemplate is the LaunchAgent plist for the menu bar tray icon.
// LSUIElement=true hides the app from the Dock.  RunAtLoad starts it at
// login.  KeepAlive is false — if the user quits the tray, it stays quit
// until next login.
const trayPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.quantifai.sync-tray</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>tray</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
	<key>LSUIElement</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`

// LaunchAgent implements the Installer interface for macOS.  It generates
// plists at ~/Library/LaunchAgents/ for both the daemon and the tray icon,
// and manages them via launchctl (bootstrap/bootout to install, unload to
// uninstall).
type LaunchAgent struct {
	plistPath     string
	trayPlistPath string
	logPath       string
	binaryPath    string
}

// NewLaunchAgent creates a LaunchAgent installer with paths derived from
// the current user's home directory.  The binary path is resolved via
// os.Executable() so the plist always points to the actual install location.
func NewLaunchAgent() *LaunchAgent {
	home, _ := os.UserHomeDir()
	binPath, err := os.Executable()
	if err != nil {
		binPath = "/usr/local/bin/quantifai-sync"
	}
	return &LaunchAgent{
		plistPath:     filepath.Join(home, "Library", "LaunchAgents", plistLabel+".plist"),
		trayPlistPath: filepath.Join(home, "Library", "LaunchAgents", trayPlistLabel+".plist"),
		logPath:       filepath.Join(home, "Library", "Logs", "quantifai-sync.log"),
		binaryPath:    binPath,
	}
}

// ConfigPath returns the plist file path.
func (la *LaunchAgent) ConfigPath() string {
	return la.plistPath
}

// GeneratePlist returns the daemon plist XML content with binary and log paths resolved.
func (la *LaunchAgent) GeneratePlist() string {
	return fmt.Sprintf(plistTemplate, la.binaryPath, la.logPath, la.logPath)
}

// GenerateTrayPlist returns the tray plist XML content with the binary path resolved.
func (la *LaunchAgent) GenerateTrayPlist() string {
	return fmt.Sprintf(trayPlistTemplate, la.binaryPath)
}

// Install writes the daemon and tray plist files and loads them via launchctl.
func (la *LaunchAgent) Install() error {
	// Ensure the LaunchAgents directory exists
	dir := filepath.Dir(la.plistPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("service: create directory %s: %w", dir, err)
	}

	// Install daemon plist
	content := la.GeneratePlist()
	if err := os.WriteFile(la.plistPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("service: write plist %s: %w", la.plistPath, err)
	}

	if err := reloadAgent(plistLabel, la.plistPath); err != nil {
		return fmt.Errorf("service: load daemon: %w", err)
	}

	// Install tray plist (best-effort — tray is optional)
	trayContent := la.GenerateTrayPlist()
	if err := os.WriteFile(la.trayPlistPath, []byte(trayContent), 0644); err == nil {
		reloadAgent(trayPlistLabel, la.trayPlistPath)
	}

	return nil
}

// bootoutTimeout bounds the wait for launchd to remove a booted-out agent.
// launchctl bootout returns before the job is gone, and launchd gives a job
// 20s (its default ExitTimeOut) to exit before SIGKILL, so wait longer.
const bootoutTimeout = 30 * time.Second

// reloadAgent starts the agent in plist, replacing any loaded instance so a
// reinstall runs the new binary and the new plist. "launchctl load" on a
// loaded label fails or leaves the old process running, and "kickstart -k"
// restarts the process without rereading a changed plist. So it boots out
// a loaded agent, waits until launchd no longer lists it (bootout is
// asynchronous, and bootstrap before the job is gone fails with
// "Bootstrap failed: 5: Input/output error"), then bootstraps.
func reloadAgent(label, plist string) error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	target := domain + "/" + label
	if agentLoaded(target) {
		if out, err := exec.Command("launchctl", "bootout", target).CombinedOutput(); err != nil && agentLoaded(target) {
			return fmt.Errorf("launchctl bootout %s: %s: %w", target, out, err)
		}
		// Bootstrap even after a timeout: returning here would leave the
		// agent booted out and stopped.
		deadline := time.Now().Add(bootoutTimeout)
		for agentLoaded(target) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap %s %s: %s: %w", domain, plist, out, err)
	}
	return nil
}

// agentLoaded reports whether launchd lists target (domain/label).
func agentLoaded(target string) bool {
	return exec.Command("launchctl", "print", target).Run() == nil
}

// Uninstall unloads both the daemon and tray agents and removes their plist files.
func (la *LaunchAgent) Uninstall() error {
	// Unload and remove daemon plist
	exec.Command("launchctl", "unload", la.plistPath).CombinedOutput()
	if err := os.Remove(la.plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove plist %s: %w", la.plistPath, err)
	}

	// Unload and remove tray plist (best-effort)
	exec.Command("launchctl", "unload", la.trayPlistPath).CombinedOutput()
	os.Remove(la.trayPlistPath)

	return nil
}

// MigrateFromOld detects the old com.ai-ops.shipper LaunchAgent plist,
// unloads it, and removes the plist file.  Returns nil if no old plist
// was found.  The old log file is renamed with a .migrated suffix.
func (la *LaunchAgent) MigrateFromOld() error {
	home, _ := os.UserHomeDir()
	oldPlist := filepath.Join(home, "Library", "LaunchAgents", legacyPlistLabel+".plist")

	if _, err := os.Stat(oldPlist); os.IsNotExist(err) {
		return nil // nothing to migrate
	}

	// Unload the old agent (best-effort)
	exec.Command("launchctl", "unload", oldPlist).CombinedOutput()

	// Remove the old plist
	if err := os.Remove(oldPlist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove legacy plist %s: %w", oldPlist, err)
	}

	// Rename old log file (best-effort)
	oldLog := filepath.Join(home, "Library", "Logs", "ai-ops-shipper.log")
	if _, err := os.Stat(oldLog); err == nil {
		os.Rename(oldLog, oldLog+".migrated")
	}

	return nil
}
