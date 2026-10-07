//go:build darwin

package tray

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/getlantern/systray"
)

const (
	plistLabel = "com.quantifai.sync"
	pollInterval = 5 * time.Second
)

// healthResponse mirrors the JSON from the health server's /healthz endpoint.
type healthResponse struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	LastSyncTime    string `json:"last_sync_time"`
	FilesTracked    int    `json:"files_tracked"`
	RecordsBuffered int    `json:"records_buffered"`
	ErrorsLastHour  int    `json:"errors_last_hour"`
	Problem         string `json:"problem,omitempty"`
}

// Run starts the systray menu bar icon. It blocks until the user quits.
// healthPort is the port where the quantifai-sync health server listens.
// dashboardURL is the URL to open when "Open Dashboard" is clicked.
// Must be called from the main goroutine on macOS.
func Run(healthPort int, dashboardURL string) {
	systray.Run(func() {
		onReady(healthPort, dashboardURL)
	}, func() {
		// onExit — nothing to clean up
	})
}

func onReady(healthPort int, dashboardURL string) {
	systray.SetIcon(iconTemplate)
	systray.SetTooltip("Quantifai Sync")

	mStatus := systray.AddMenuItem("Quantifai Sync — Starting...", "Pipeline status")
	mStatus.Disable()

	systray.AddSeparator()

	mLastSync := systray.AddMenuItem("Last sync: —", "Last successful sync time")
	mLastSync.Disable()
	mFiles := systray.AddMenuItem("Files tracked: 0", "Number of watched JSONL files")
	mFiles.Disable()
	mBuffered := systray.AddMenuItem("Records buffered: 0", "Records waiting to be sent")
	mBuffered.Disable()
	mErrors := systray.AddMenuItem("Errors: 0", "Errors in the last hour")
	mErrors.Disable()

	systray.AddSeparator()

	// --- Controls ---
	mStartStop := systray.AddMenuItem("Stop Daemon", "Start or stop the background sync daemon")
	mRestart := systray.AddMenuItem("Restart Daemon", "Restart the background sync daemon")

	systray.AddSeparator()

	mDashboard := systray.AddMenuItem("Open Dashboard...", "Open Quantifai dashboard in browser")
	mLogs := systray.AddMenuItem("View Logs...", "Open log file in Console.app")

	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit Tray", "Quit the menu bar icon (daemon keeps running)")

	// Track daemon state
	daemonRunning := true
	lastErrorCount := 0
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", healthPort)

	// Poll health endpoint
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for {
			h, running := fetchHealth(healthURL)
			daemonRunning = running

			if running {
				updateMenuRunning(h, mStatus, mLastSync, mFiles, mBuffered, mErrors)
				mStartStop.SetTitle("Stop Daemon")

				// Notify on new errors
				if h.ErrorsLastHour > lastErrorCount && lastErrorCount >= 0 {
					newErrors := h.ErrorsLastHour - lastErrorCount
					notify("Quantifai Sync", fmt.Sprintf("%d new error(s) in the last hour", newErrors))
				}
				lastErrorCount = h.ErrorsLastHour

				// Notify on degraded/error status transitions
				if h.Status == "error" {
					mStatus.SetTitle(statusTitle(h))
				}
			} else {
				mStatus.SetTitle("Quantifai Sync — Not Running")
				mLastSync.SetTitle("Last sync: —")
				mFiles.SetTitle("Files tracked: —")
				mBuffered.SetTitle("Records buffered: —")
				mErrors.SetTitle("Errors: —")
				mStartStop.SetTitle("Start Daemon")
				lastErrorCount = 0
			}

			<-ticker.C
		}
	}()

	// Handle menu item clicks
	go func() {
		for {
			select {
			case <-mStartStop.ClickedCh:
				if daemonRunning {
					stopDaemon()
					notify("Quantifai Sync", "Daemon stopped")
				} else {
					startDaemon()
					notify("Quantifai Sync", "Daemon starting...")
				}

			case <-mRestart.ClickedCh:
				stopDaemon()
				time.Sleep(1 * time.Second)
				startDaemon()
				notify("Quantifai Sync", "Daemon restarted")

			case <-mDashboard.ClickedCh:
				exec.Command("open", dashboardURL).Start()

			case <-mLogs.ClickedCh:
				logPath := logFilePath()
				if logPath != "" {
					exec.Command("open", "-a", "Console", logPath).Start()
				} else {
					exec.Command("open", "-a", "Console").Start()
				}

			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func updateMenuRunning(h *healthResponse, mStatus, mLastSync, mFiles, mBuffered, mErrors *systray.MenuItem) {
	mStatus.SetTitle(statusTitle(h))

	// Last sync
	if h.LastSyncTime != "" {
		t, err := time.Parse(time.RFC3339, h.LastSyncTime)
		if err == nil {
			ago := time.Since(t).Truncate(time.Second)
			mLastSync.SetTitle(fmt.Sprintf("Last sync: %s ago", formatDuration(ago)))
		} else {
			mLastSync.SetTitle(fmt.Sprintf("Last sync: %s", h.LastSyncTime))
		}
	} else {
		mLastSync.SetTitle("Last sync: never")
	}

	// Counters
	mFiles.SetTitle(fmt.Sprintf("Files tracked: %d", h.FilesTracked))
	mBuffered.SetTitle(fmt.Sprintf("Records buffered: %d", h.RecordsBuffered))
	mErrors.SetTitle(fmt.Sprintf("Errors (last hour): %d", h.ErrorsLastHour))
}

// fetchHealth queries the healthz endpoint. Returns nil and false if unreachable.
func fetchHealth(healthURL string) (*healthResponse, bool) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(healthURL)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}

	var h healthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, false
	}
	return &h, true
}

// startDaemon loads the LaunchAgent plist to start the daemon.
func startDaemon() {
	plist := plistPath()
	if plist == "" {
		return
	}
	exec.Command("launchctl", "load", plist).Run()
}

// stopDaemon unloads the LaunchAgent plist to stop the daemon.
func stopDaemon() {
	plist := plistPath()
	if plist == "" {
		return
	}
	exec.Command("launchctl", "unload", plist).Run()
}

// plistPath returns the path to the daemon LaunchAgent plist.
func plistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", plistLabel+".plist")
}

// logFilePath returns the path to the daemon log file.
func logFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, "Library", "Logs", "quantifai-sync.log")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

// notify sends a macOS notification via osascript.
func notify(title, message string) {
	script := fmt.Sprintf(`display notification %q with title %q`, message, title)
	exec.Command("osascript", "-e", script).Start()
}

// formatDuration returns a human-friendly duration like "2m 30s" or "1h 5m".
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s > 0 {
			return fmt.Sprintf("%dm %ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dh", h)
}

// isDaemonLoaded checks if the LaunchAgent is currently loaded.
func isDaemonLoaded() bool {
	out, err := exec.Command("launchctl", "list").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), plistLabel)
}
