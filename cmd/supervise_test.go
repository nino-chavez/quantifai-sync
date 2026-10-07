package cmd

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quantifai/sync/internal/logger"
	"github.com/quantifai/sync/internal/updater"
)

// TestSuperviseHelper is not a test: the supervisor tests run copies of the
// test binary on only this function, as the child or the supervisor.
func TestSuperviseHelper(t *testing.T) {
	mode, arg, _ := strings.Cut(os.Getenv("QUANTIFAI_SUPERVISE_HELPER"), ":")
	switch mode {
	case "crash-twice":
		// Each run appends a mark; the first two runs fail.
		f, _ := os.OpenFile(arg, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		f.WriteString("x")
		f.Close()
		b, _ := os.ReadFile(arg)
		if len(b) < 3 {
			os.Exit(1)
		}
		os.Exit(0)
	case "update-then-verify":
		// Records the release each run received. The first run "installs"
		// v9.9.9; the helper stops after three runs so a broken handoff
		// shows as extra lines instead of looping forever.
		f, _ := os.OpenFile(arg, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		f.WriteString("[" + os.Getenv(updater.UpdatedToEnv) + "]")
		f.Close()
		b, _ := os.ReadFile(arg)
		switch {
		case strings.Count(string(b), "[") >= 3, os.Getenv(updater.UpdatedToEnv) == "v9.9.9":
			os.Exit(0)
		}
		os.WriteFile(os.Getenv(handoffEnv), []byte("v9.9.9"), 0600)
		os.Exit(exitUpdated)
	case "sleep":
		os.WriteFile(arg, []byte(strconv.Itoa(os.Getpid())), 0600)
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "supervisor":
		if err := killChildrenWithSupervisor(); err != nil {
			os.Exit(2)
		}
		os.Setenv("QUANTIFAI_SUPERVISE_HELPER", "sleep:"+arg)
		l, _ := logger.New(logger.LevelError, "")
		os.Exit(supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 10*time.Millisecond, l))
	}
}

// The supervisor restarts a child that exits with an error and stops once
// it exits cleanly.
func TestSuperviseRestartsUntilCleanExit(t *testing.T) {
	marks := t.TempDir() + "/runs"
	t.Setenv("QUANTIFAI_SUPERVISE_HELPER", "crash-twice:"+marks)
	l, _ := logger.New(logger.LevelError, "")

	code := supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 10*time.Millisecond, l)
	if code != 0 {
		t.Fatalf("supervise returned %d, want 0 after the child exits cleanly", code)
	}
	if b, _ := os.ReadFile(marks); string(b) != "xxx" {
		t.Fatalf("child ran %d times, want 3 (two crashes, then a clean exit)", len(b))
	}
}

// After the child exits with exitUpdated, the supervisor starts it again at
// once (no crash delay) and passes on the release it installed, so the new
// child's updater will not install that release again.
func TestSuperviseRestartsAtOnceAfterUpdate(t *testing.T) {
	runs := t.TempDir() + "/runs"
	t.Setenv("QUANTIFAI_SUPERVISE_HELPER", "update-then-verify:"+runs)
	t.Setenv(updater.UpdatedToEnv, "")
	l, _ := logger.New(logger.LevelError, "")

	start := time.Now()
	code := supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 20*time.Second, l)
	elapsed := time.Since(start)
	got, _ := os.ReadFile(runs)
	if code != 0 || string(got) != "[][v9.9.9]" {
		t.Fatalf("runs received %s (exit %d), want [][v9.9.9]: the restarted child must get the installed release", got, code)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("restart after an update took %v; it must not wait for the 20s crash delay", elapsed)
	}
}
