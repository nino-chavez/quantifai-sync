package cmd

import (
	"os"
	"os/exec"
	"time"

	"github.com/quantifai/sync/internal/config"
	"github.com/quantifai/sync/internal/logger"
)

// supervisorRestartDelay is the wait before restarting a failed agent,
// matching launchd's ThrottleInterval and systemd's RestartSec.
const supervisorRestartDelay = 10 * time.Second

// superviseAgent runs the agent ("run") as a child process and restarts it
// when it exits with an error. The Windows logon task starts the agent
// this way because Task Scheduler does not restart a task whose program
// exits with an error. launchd and systemd restart the agent themselves.
func superviseAgent() int {
	log := supervisorLogger()
	// Resolved once: after a self-update renames the running file aside,
	// this path holds the new binary.
	exe, err := os.Executable()
	if err != nil {
		log.Error("supervisor could not locate its executable", map[string]any{"error": err.Error()})
		return 1
	}
	if err := killChildrenWithSupervisor(); err != nil {
		log.Warn("supervisor could not tie the agent's lifetime to its own", map[string]any{"error": err.Error()})
	}
	return supervise(exe, []string{"run"}, supervisorRestartDelay, log)
}

// supervise starts exe with args, and starts it again after delay each time
// it exits with an error. It returns 0 once the child exits cleanly.
func supervise(exe string, args []string, delay time.Duration, log *logger.Logger) int {
	for {
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		hideChildWindow(cmd)
		err := cmd.Run()
		if err == nil {
			log.Info("agent exited cleanly; supervisor stopping", nil)
			return 0
		}
		log.Warn("agent exited with an error; restarting", map[string]any{
			"error": err.Error(),
			"delay": delay.String(),
		})
		time.Sleep(delay)
	}
}

// supervisorLogger logs where the agent does, falling back to stderr.
func supervisorLogger() *logger.Logger {
	if cfg, err := config.Load("", ""); err == nil {
		if l, err := logger.New(logger.ParseLevel(cfg.LogLevel), cfg.LogFile); err == nil {
			return l
		}
	}
	l, _ := logger.New(logger.LevelInfo, "")
	return l
}
