//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// When the supervisor is killed outright, as Task Scheduler ends a task,
// the agent it started must not survive it.
func TestSupervisorDeathKillsChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	sup := exec.Command(os.Args[0], "-test.run=^TestSuperviseHelper$")
	sup.Env = append(os.Environ(), "QUANTIFAI_SUPERVISE_HELPER=supervisor:"+pidFile)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Process.Kill()

	var childPID int
	for i := 0; i < 100 && childPID == 0; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			childPID, _ = strconv.Atoi(string(b))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("supervised child never started")
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(childPID))
	if err != nil {
		t.Fatalf("open child %d: %v", childPID, err)
	}
	defer windows.CloseHandle(child)

	sup.Process.Kill()
	sup.Wait()

	if ev, _ := windows.WaitForSingleObject(child, 5000); ev != windows.WAIT_OBJECT_0 {
		windows.TerminateProcess(child, 1)
		t.Fatalf("child %d still running 5s after its supervisor was killed", childPID)
	}
}

// With no console, the agent's stderr (where a config problem it is waiting
// on is reported) and the supervisor's own messages must reach the log file.
func TestSupervisorLogsAgentOutput(t *testing.T) {
	appData := t.TempDir()
	sup := exec.Command(os.Args[0], "-test.run=^TestSuperviseHelper$")
	sup.Env = append(os.Environ(), "QUANTIFAI_SUPERVISE_HELPER=supervise-echo", "LOCALAPPDATA="+appData)
	if out, err := sup.CombinedOutput(); err != nil {
		t.Fatalf("supervisor: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(appData, "quantifai", "quantifai-sync.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hello from the agent", "agent exited cleanly"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log missing %q:\n%s", want, b)
		}
	}
}

// A supervisor that panics after redirecting its output must leave the
// trace in the log. The runtime writes it to the process's standard error
// handle, not to os.Stderr, and the supervisor has no console to show it.
func TestSupervisorPanicGoesToLog(t *testing.T) {
	appData := t.TempDir()
	sup := exec.Command(os.Args[0], "-test.run=^TestSuperviseHelper$")
	sup.Env = append(os.Environ(), "QUANTIFAI_SUPERVISE_HELPER=redirect-then-panic", "LOCALAPPDATA="+appData)
	out, err := sup.CombinedOutput()
	if err == nil {
		t.Fatalf("helper did not panic:\n%s", out)
	}
	b, _ := os.ReadFile(filepath.Join(appData, "quantifai", "quantifai-sync.log"))
	for _, want := range []string{"panic: supervisor test panic", "goroutine "} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log missing %q\nlog:\n%s\nprocess output:\n%s", want, b, out)
		}
	}
}
