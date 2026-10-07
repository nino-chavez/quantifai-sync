//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
