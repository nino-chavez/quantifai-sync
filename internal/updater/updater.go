// Package updater provides opt-in self-update from GitHub Releases.
// When auto_update is disabled (the default), a no-op implementation is
// used that never checks for updates.  When enabled, a GithubUpdater
// checks on startup and periodically, downloading and atomically
// replacing the binary when a newer version is found.
package updater

import (
	"context"
	"time"

	"github.com/quantifai/sync/internal/logger"
)

// defaultCheckInterval is the fallback interval between update checks.
const defaultCheckInterval = 24 * time.Hour

// Updater defines the interface for checking and applying updates.
// Implementations must be safe for concurrent use.
type Updater interface {
	// CheckAndApply checks for a newer version and applies it if found.
	// It returns true if an update was applied. The running process is
	// still the old binary until it restarts.
	CheckAndApply(ctx context.Context) (applied bool, err error)

	// Run starts the background update loop.  It checks immediately on
	// start, then periodically.  It blocks until ctx is cancelled, or
	// returns after applying an update.
	Run(ctx context.Context)

	// Applied receives once when Run has installed an update, so the
	// caller can restart into the new binary. Nil when updates are off.
	Applied() <-chan struct{}
}

// NoopUpdater is returned when auto_update is false.  It never checks
// for updates and all methods return immediately.
type NoopUpdater struct{}

// CheckAndApply always returns false with no error (no-op).
func (n *NoopUpdater) CheckAndApply(_ context.Context) (bool, error) {
	return false, nil
}

// Run returns immediately because auto-update is disabled.
func (n *NoopUpdater) Run(_ context.Context) {}

// Applied returns nil: a receive from it never fires.
func (n *NoopUpdater) Applied() <-chan struct{} { return nil }

// NewUpdater returns the appropriate Updater implementation based on
// the autoUpdate flag.  When disabled, a NoopUpdater is returned.
// When enabled, a GithubUpdater is returned that checks for and applies
// real updates from GitHub Releases.
//
// The repo parameter is "owner/repo" (e.g. "quantifai/sync").
// The checkInterval parameter is parsed as a time.Duration string
// (e.g. "24h", "12h"); defaults to 24h if empty or invalid.
func NewUpdater(autoUpdate bool, version, updateChannel, repo, checkInterval string, log *logger.Logger) Updater {
	if !autoUpdate {
		return &NoopUpdater{}
	}

	interval := defaultCheckInterval
	if checkInterval != "" {
		if d, err := time.ParseDuration(checkInterval); err == nil && d > 0 {
			interval = d
		}
	}

	if repo == "" {
		repo = "quantifai-app/sync"
	}

	return NewGithubUpdater(version, updateChannel, repo, interval, log)
}
