//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRestartHelper is not a test: TestRestartInPlaceKeepsPID runs a copy
// of the test binary on only this function. Stage 1 restarts in place into
// stage 2; both print their PID.
func TestRestartHelper(t *testing.T) {
	switch os.Getenv("QUANTIFAI_RESTART_HELPER") {
	case "1":
		fmt.Printf("stage1 %d\n", os.Getpid())
		os.Setenv("QUANTIFAI_RESTART_HELPER", "2")
		err := restartInPlace()
		fmt.Printf("restart failed: %v\n", err)
		os.Exit(3)
	case "2":
		fmt.Printf("stage2 %d\n", os.Getpid())
		os.Exit(0)
	}
}

func TestRestartInPlaceKeepsPID(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestartHelper$")
	cmd.Env = append(os.Environ(), "QUANTIFAI_RESTART_HELPER=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	pid := cmd.Process.Pid
	want := fmt.Sprintf("stage1 %d\nstage2 %d\n", pid, pid)
	if !strings.Contains(string(out), want) {
		t.Fatalf("output %q, want both stages from PID %d", out, pid)
	}
}
