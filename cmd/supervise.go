package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/quantifai/sync/internal/config"
	"github.com/quantifai/sync/internal/logger"
	"github.com/quantifai/sync/internal/service"
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
// version than its tag would be installed again on every start. The file
// outlives the supervisor, so a new instance of the logon task gets the
// tag too.
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
	handoff := service.SupervisorHandoffPath()
	if err := os.MkdirAll(filepath.Dir(handoff), 0700); err != nil {
		// Without it an update could not hand its release on, and a
		// misstamped release would be installed again on every start.
		log.Error("supervisor could not create its handoff folder", map[string]any{"error": err.Error()})
		return 1
	}
	return supervise(exe, []string{"run"}, supervisorRestartDelay, handoff, restartLogonTask, log)
}

// supervise starts exe with args, and starts it again after delay each time
// it exits with an error, or at once when it exits after an update. It
// returns 0 once the child exits cleanly.
//
// handoff is the file a child that installed an update writes the release
// to. A tag already there at start was left by the supervisor this one
// replaced, and goes to the first child.
//
// After an update the running supervisor is still the old binary: the
// update renamed its file aside. restartTask, when set, asks for a new
// supervisor on the updated binary. A new supervisor takes the release out
// of handoff as it starts, so the file disappearing within handoverWait is
// the proof it is running; this supervisor then returns. If restartTask
// fails or the file stays, this supervisor starts the new agent itself, as
// before.
func supervise(exe string, args []string, delay time.Duration, handoff string, restartTask func() error, log *logger.Logger) int {
	updatedTo := takeHandoff(handoff)
	if updatedTo != "" {
		log.Info("supervisor started after an update", map[string]any{"release": updatedTo})
	}

	for {
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), handoffEnv+"="+handoff)
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
			b, _ := os.ReadFile(handoff)
			updatedTo = strings.TrimSpace(string(b))
			if restartTask != nil {
				log.Info("agent installed an update; restarting the supervisor on the new binary", map[string]any{"release": updatedTo})
				err := handOver(handoff, updatedTo, restartTask)
				if err == nil {
					log.Info("a new supervisor is running; this one is stopping", nil)
					return 0
				}
				log.Warn("could not restart the supervisor; starting the new agent from this one", map[string]any{"error": err.Error()})
			} else {
				log.Info("agent installed an update; starting the new binary", map[string]any{"release": updatedTo})
			}
			os.Remove(handoff)
			continue
		}
		log.Warn("agent exited with an error; restarting", map[string]any{
			"error": err.Error(),
			"delay": delay.String(),
		})
		time.Sleep(delay)
	}
}

// handoverWait is how long a supervisor waits for its replacement to take
// the handoff file.
var handoverWait = 30 * time.Second

// handOver leaves updatedTo in handoff for a new supervisor, starts one with
// restartTask, and waits for it to take the file. A supervisor started by
// the logon task is usually ended by Task Scheduler during the wait.
func handOver(handoff, updatedTo string, restartTask func() error) error {
	// Written here as well, in case the agent could not: the file's
	// presence is what the new supervisor's start is measured by.
	if err := os.WriteFile(handoff, []byte(updatedTo), 0600); err != nil {
		return err
	}
	if err := restartTask(); err != nil {
		return err
	}
	for deadline := time.Now().Add(handoverWait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(handoff); os.IsNotExist(err) {
			return nil
		}
	}
	return fmt.Errorf("no new supervisor took the handoff within %s", handoverWait)
}

// takeHandoff returns the release left in handoff, if any, and removes
// the file so a later start does not reuse it.
func takeHandoff(handoff string) string {
	b, err := os.ReadFile(handoff)
	if err != nil {
		return ""
	}
	os.Remove(handoff)
	return strings.TrimSpace(string(b))
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
