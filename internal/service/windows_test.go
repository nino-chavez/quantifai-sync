//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// testTask registers a task under a name only this test uses and deletes
// it when the test ends, so running the tests never leaves a real
// QuantifaiSync task behind.
func testTask(t *testing.T, binPath, args string) *Windows {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	w := &Windows{taskName: fmt.Sprintf("QuantifaiSyncTest-%d", os.Getpid()), binPath: binPath, args: args, user: u.Username}
	t.Cleanup(func() {
		exec.Command("schtasks", "/end", "/tn", w.taskName).CombinedOutput()
		exec.Command("schtasks", "/delete", "/tn", w.taskName, "/f").CombinedOutput()
	})
	if err := w.register(); err != nil {
		t.Fatal(err)
	}
	return w
}

// decode returns schtasks output as text; /xml output is UTF-16.
func decode(b []byte) string {
	if len(b) >= 2 && (b[0] == 0xFF && b[1] == 0xFE || b[1] == 0) {
		if b[0] == 0xFF {
			b = b[2:]
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

// What Task Scheduler actually stored, read back with its own export.
func TestTaskRegistersWithLongRunningSettings(t *testing.T) {
	bin := `C:\Program Files\quantifai\quantifai-sync.exe`
	w := testTask(t, bin, taskArguments)

	out, err := exec.Command("schtasks", "/query", "/tn", w.taskName, "/xml").Output()
	if err != nil {
		t.Fatal(err)
	}
	stored := decode(out)
	for _, want := range []string{
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<LogonTrigger>",
		"<Command>" + bin + "</Command>",
		"<Arguments>run --no-console</Arguments>",
	} {
		if !strings.Contains(stored, want) {
			t.Errorf("stored task is missing %s", want)
		}
	}
	if t.Failed() {
		t.Logf("stored task:\n%s", stored)
	}
}

// Whether the task starts as this user. CI may have no interactive
// session for an InteractiveToken task, so a task that never runs is
// reported and skipped rather than failed.
func TestTaskRunsAsInstallingUser(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran.txt")
	w := testTask(t, `C:\Windows\System32\cmd.exe`, fmt.Sprintf(`/c echo %%USERNAME%%> "%s"`, marker))

	if out, err := exec.Command("schtasks", "/run", "/tn", w.taskName).CombinedOutput(); err != nil {
		t.Fatalf("schtasks /run: %s: %v", out, err)
	}
	for i := 0; i < 40; i++ {
		if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
			got := strings.TrimSpace(string(b))
			if !strings.HasSuffix(strings.ToLower(w.user), strings.ToLower(`\`+got)) && !strings.EqualFold(w.user, got) {
				t.Fatalf("task ran as %q, want %q", got, w.user)
			}
			t.Logf("task ran as %s", got)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	info, _ := exec.Command("schtasks", "/query", "/tn", w.taskName, "/v", "/fo", "list").CombinedOutput()
	t.Skipf("task did not run within 20s; scheduler state:\n%s", info)
}
