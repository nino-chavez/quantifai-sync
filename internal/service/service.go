// Package service provides platform-specific service installation and
// uninstallation for macOS LaunchAgent, Linux systemd user units, and a
// Windows Task Scheduler logon task.  Each platform implementation conforms to the
// Installer interface and optionally the Migrator interface for upgrading
// from the old ai-ops-shipper service.
package service

import "fmt"

// Installer defines the interface for platform-specific service management.
type Installer interface {
	// Install generates the service configuration file and registers/starts
	// the service with the platform's service manager.
	Install() error

	// Uninstall stops the service, removes the configuration file, and
	// unregisters it from the platform's service manager.
	Uninstall() error

	// ConfigPath returns the filesystem path of the generated service
	// configuration file (plist, unit file, etc.).
	ConfigPath() string
}

// Migrator defines the interface for migrating from the old ai-ops-shipper
// service to quantifai-sync.  Platform implementations that support
// migration implement this interface.
type Migrator interface {
	// MigrateFromOld detects and removes the old ai-ops-shipper service
	// configuration, making way for the new quantifai-sync service.
	// Returns nil if no old service was found.
	MigrateFromOld() error
}

// newWindowsInstaller is a hook that the build-tag-gated windows.go file
// sets during init() to make the Windows installer available without
// importing Windows-specific types on other platforms.
var newWindowsInstaller func() Installer

// NewInstaller returns the appropriate Installer for the given platform
// (runtime.GOOS value).  Supported platforms: darwin, linux, windows.
func NewInstaller(platform string) (Installer, error) {
	switch platform {
	case "darwin":
		return NewLaunchAgent(), nil
	case "linux":
		return NewSystemd(), nil
	case "windows":
		if newWindowsInstaller != nil {
			return newWindowsInstaller(), nil
		}
		return nil, fmt.Errorf("service: windows support not compiled into this binary")
	default:
		return nil, fmt.Errorf("service: unsupported platform %q", platform)
	}
}
