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
	"github.com/quantifai/sync/internal/filelock"
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
	// One supervisor at a time. A new instance of the logon task waits here
	// until the one it replaces has exited, so two never run agents side by
	// side, and it cannot take the handoff from a supervisor still running.
	if _, err := lockSupervisor(filepath.Join(filepath.Dir(handoff), "supervisor.lock")); err != nil {
		log.Warn("supervisor could not take its lock", map[string]any{"error": err.Error()})
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
// supervisor on the updated binary, which ends this one (see handOver). If
// restartTask fails, or this supervisor is still running after
// handoverWait, it starts the new agent itself, as before.
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
				log.Warn("the supervisor was not replaced; starting the new agent from this one", map[string]any{"error": err.Error()})
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

// handoverWait is how long a supervisor waits to be replaced after asking
// for a new one.
var handoverWait = 30 * time.Second

// handOver leaves updatedTo in handoff for a new supervisor and starts one
// with restartTask. For a supervisor started by the logon task, Task
// Scheduler ends it during the wait, and the new one, blocked on the
// supervisor lock until then, takes the handoff. It returns an error if
// that does not happen: restartTask failed, or this supervisor was not
// started by the task (the new one waits on the lock instead).
func handOver(handoff, updatedTo string, restartTask func() error) error {
	if err := os.WriteFile(handoff, []byte(updatedTo), 0600); err != nil {
		return err
	}
	if err := restartTask(); err != nil {
		return err
	}
	time.Sleep(handoverWait)
	return fmt.Errorf("still running %s after starting a new supervisor", handoverWait)
}

// lockSupervisor takes the supervisor lock at path, blocking until any
// other supervisor holding it has exited. The lock is held for the life of
// the process; the system releases it however the process ends.
func lockSupervisor(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := filelock.Lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
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
