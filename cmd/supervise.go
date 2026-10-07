package cmd

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/quantifai/sync/internal/config"
	"github.com/quantifai/sync/internal/logger"
	"github.com/quantifai/sync/internal/updater"
)

// supervisorRestartDelay is the wait before restarting a failed agent,
// matching launchd's ThrottleInterval and systemd's RestartSec.
const supervisorRestartDelay = 10 * time.Second

// exitUpdated is the agent's exit code after installing an update under a
// supervisor: start the new binary now, not after the crash delay.
const exitUpdated = 3

// handoffEnv names a file the supervisor gives each child. A child that
// installs an update writes the release tag there before exiting, and the
// supervisor passes it on as updater.UpdatedToEnv, which exec carries on
// macOS and Linux. Without it, a release whose binary reports an older
// version than its tag would be installed again on every start.
const handoffEnv = "QUANTIFAI_SUPERVISOR_HANDOFF"

// superviseAgent runs the agent ("run") as a child process and restarts it
// when it exits with an error. The Windows logon task starts the agent
// this way because Task Scheduler does not restart a task whose program
// exits with an error. launchd and systemd restart the agent themselves.
func superviseAgent() int {
	// The logon task has no console: send this process's output, and the
	// agent's, which inherits it, to a log file first.
	logErr := redirectOutputToLog()
	log := supervisorLogger()
	if logErr != nil {
		log.Warn("supervisor could not open its log file", map[string]any{"error": logErr.Error()})
	}
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
// it exits with an error, or at once when it exits after an update. It
// returns 0 once the child exits cleanly.
func supervise(exe string, args []string, delay time.Duration, log *logger.Logger) int {
	handoff, err := os.CreateTemp("", "quantifai-sync-handoff-*")
	if err != nil {
		log.Error("supervisor could not create its handoff file", map[string]any{"error": err.Error()})
		return 1
	}
	handoff.Close()
	defer os.Remove(handoff.Name())
	updatedTo := ""

	for {
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), handoffEnv+"="+handoff.Name())
		if updatedTo != "" {
			cmd.Env = append(cmd.Env, updater.UpdatedToEnv+"="+updatedTo)
		}
		hideChildWindow(cmd)
		err := cmd.Run()
		if err == nil {
			log.Info("agent exited cleanly; supervisor stopping", nil)
			return 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == exitUpdated {
			b, _ := os.ReadFile(handoff.Name())
			updatedTo = strings.TrimSpace(string(b))
			log.Info("agent installed an update; starting the new binary", map[string]any{"release": updatedTo})
			continue
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
