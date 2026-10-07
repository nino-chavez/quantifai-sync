package cmd

import (
	"fmt"
	"runtime"

	"github.com/quantifai/sync/internal/credentials"
	"github.com/quantifai/sync/internal/service"
)

// RunInstall detects the current platform, generates the appropriate
// service configuration, and installs/enables the background service.
// Before installing, it migrates from any old ai-ops-shipper service
// if one is detected.  The apiKey parameter is the value from --api-key;
// empty means the caller should have already stored the key via keyring
// or config.
func RunInstall(apiKey string) int {
	// Store API key in OS keyring if provided
	if apiKey != "" {
		mgr := credentials.NewManagerWithOSKeyring("")
		if err := mgr.StoreAPIKey(apiKey); err != nil {
			fmt.Printf("warning: could not store API key in keyring: %v\n", err)
		} else {
			fmt.Println("API key stored in OS keyring (the agent reads it before any api_key in config.toml)")
		}
	}

	platform := runtime.GOOS

	installer, err := service.NewInstaller(platform)
	if err != nil {
		fmt.Printf("install failed: %v\n", err)
		return 1
	}

	// Migrate from old ai-ops-shipper service if present
	if migrator, ok := installer.(service.Migrator); ok {
		if err := migrator.MigrateFromOld(); err != nil {
			fmt.Printf("warning: migration from old service failed: %v\n", err)
			// Continue with install anyway
		} else {
			fmt.Println("migrated from old ai-ops-shipper service")
		}
	}

	if err := installer.Install(); err != nil {
		fmt.Printf("install failed: %v\n", err)
		return 1
	}

	fmt.Printf("quantifai-sync installed successfully on %s\n", platform)
	fmt.Println("tip: you can safely delete the old ai-ops-shipper binary if it still exists")
	return 0
}
