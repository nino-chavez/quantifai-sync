package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	case "echo":
		os.Stderr.WriteString("hello from the agent\n")
		os.Exit(0)
	case "supervise-echo":
		if err := redirectOutputToLog(); err != nil {
			os.Exit(2)
		}
		os.Setenv("QUANTIFAI_SUPERVISE_HELPER", "echo")
		l, _ := logger.New(logger.LevelInfo, "")
		os.Exit(supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 10*time.Millisecond, helperHandoff(), nil, l))
	case "redirect-then-panic":
		if err := redirectOutputToLog(); err != nil {
			os.Exit(2)
		}
		panic("supervisor test panic")
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
		os.Exit(supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 10*time.Millisecond, helperHandoff(), nil, l))
	}
}

// helperHandoff is a handoff file for a supervisor run as a test helper.
func helperHandoff() string {
	return filepath.Join(os.TempDir(), "quantifai-sync-test-handoff-"+strconv.Itoa(os.Getpid()))
}

// The supervisor restarts a child that exits with an error and stops once
// it exits cleanly.
func TestSuperviseRestartsUntilCleanExit(t *testing.T) {
	marks := t.TempDir() + "/runs"
	t.Setenv("QUANTIFAI_SUPERVISE_HELPER", "crash-twice:"+marks)
	l, _ := logger.New(logger.LevelError, "")

	code := supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 10*time.Millisecond, t.TempDir()+"/handoff", nil, l)
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
	code := supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 20*time.Second, t.TempDir()+"/handoff", nil, l)
	elapsed := time.Since(start)
	got, _ := os.ReadFile(runs)
	if code != 0 || string(got) != "[][v9.9.9]" {
		t.Fatalf("runs received %s (exit %d), want [][v9.9.9]: the restarted child must get the installed release", got, code)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("restart after an update took %v; it must not wait for the 20s crash delay", elapsed)
	}
}

// After an update, a supervisor that can start a new one on the updated
// binary (the Windows logon task) leaves the installed release in the
// handoff file and starts it. Task Scheduler ends a task-started supervisor
// at that point (Windows CI covers it); one still running after the wait
// starts the new agent itself.
func TestSuperviseHandsOverThenFallsBack(t *testing.T) {
	runs := t.TempDir() + "/runs"
	handoff := t.TempDir() + "/handoff"
	t.Setenv("QUANTIFAI_SUPERVISE_HELPER", "update-then-verify:"+runs)
	t.Setenv(updater.UpdatedToEnv, "")
	l, _ := logger.New(logger.LevelError, "")
	defer func(w time.Duration) { handoverWait = w }(handoverWait)
	handoverWait = 200 * time.Millisecond

	inHandoff := ""
	code := supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 20*time.Second, handoff, func() error {
		b, _ := os.ReadFile(handoff)
		inHandoff = string(b)
		return nil
	}, l)
	if inHandoff != "v9.9.9" {
		t.Fatalf("handoff held %q when the new supervisor was started, want v9.9.9", inHandoff)
	}
	if got, _ := os.ReadFile(runs); code != 0 || string(got) != "[][v9.9.9]" {
		t.Fatalf("runs received %s (exit %d), want [][v9.9.9]", got, code)
	}
	if _, err := os.Stat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff file left behind (err %v)", err)
	}
}

// Only one supervisor holds the lock; a second waits until the first exits.
func TestSupervisorLockIsExclusive(t *testing.T) {
	path := t.TempDir() + "/supervisor.lock"
	first, err := lockSupervisor(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		f, err := lockSupervisor(path)
		if err == nil {
			defer f.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("second supervisor took the lock while the first held it (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	first.Close()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second supervisor: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second supervisor still waiting after the first released the lock")
	}
}

// The lock outlasts garbage collection: nothing else refers to the file,
// and a collected *os.File is closed, which would release the lock.
func TestSupervisorLockSurvivesGC(t *testing.T) {
	path := t.TempDir() + "/supervisor.lock"
	if err := holdSupervisorLock(path); err != nil {
		t.Fatal(err)
	}
	defer func() { supervisorLock.Close(); supervisorLock = nil }()
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	got := make(chan error, 1)
	go func() {
		f, err := lockSupervisor(path)
		if err == nil {
			f.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a second supervisor took the lock after garbage collection (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	supervisorLock.Close()
	<-got
}

// If starting a new supervisor fails, this one starts the new agent itself
// and passes on the release, as before.
func TestSuperviseRestartsAgentWhenHandoverFails(t *testing.T) {
	runs := t.TempDir() + "/runs"
	handoff := t.TempDir() + "/handoff"
	t.Setenv("QUANTIFAI_SUPERVISE_HELPER", "update-then-verify:"+runs)
	t.Setenv(updater.UpdatedToEnv, "")
	l, _ := logger.New(logger.LevelError, "")

	code := supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 20*time.Second, handoff, func() error {
		return errors.New("task not found")
	}, l)
	if got, _ := os.ReadFile(runs); code != 0 || string(got) != "[][v9.9.9]" {
		t.Fatalf("runs received %s (exit %d), want [][v9.9.9]", got, code)
	}
}

// A supervisor started in place of one that handed over gives the release
// it finds in the handoff file to its first agent, and removes the file.
func TestSuperviseStartsWithHandedOverRelease(t *testing.T) {
	runs := t.TempDir() + "/runs"
	handoff := t.TempDir() + "/handoff"
	if err := os.WriteFile(handoff, []byte("v9.9.9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUANTIFAI_SUPERVISE_HELPER", "update-then-verify:"+runs)
	t.Setenv(updater.UpdatedToEnv, "")
	l, _ := logger.New(logger.LevelError, "")

	code := supervise(os.Args[0], []string{"-test.run=^TestSuperviseHelper$"}, 20*time.Second, handoff, func() error {
		t.Error("restarted the supervisor, but the first agent should not install anything")
		return nil
	}, l)
	if got, _ := os.ReadFile(runs); code != 0 || string(got) != "[v9.9.9]" {
		t.Fatalf("runs received %s (exit %d), want [v9.9.9]: the first agent must get the handed-over release", got, code)
	}
	if _, err := os.Stat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff file still there after start (err %v); a later start would reuse it", err)
	}
}
