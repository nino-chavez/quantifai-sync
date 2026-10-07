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
		"<MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>",
		"<LogonTrigger>",
		"<Command>" + bin + "</Command>",
		"<Arguments>run --no-console --supervise</Arguments>",
	} {
		if !strings.Contains(stored, want) {
			t.Errorf("stored task is missing %s", want)
		}
	}
	// Windows omits RunLevel when it is the default, LeastPrivilege.
	if strings.Contains(stored, "<RunLevel>HighestAvailable</RunLevel>") {
		t.Error("stored task runs elevated, want LeastPrivilege")
	}
	if t.Failed() {
		t.Logf("stored task:\n%s", stored)
	}

	// The same limit in Windows' own words.
	info, _ := exec.Command("schtasks", "/query", "/tn", w.taskName, "/v", "/fo", "list").CombinedOutput()
	for _, line := range strings.Split(string(info), "\n") {
		if strings.Contains(line, "Stop Task If Runs") || strings.Contains(line, "Power Management") || strings.Contains(line, "Logon Mode") {
			t.Logf("%s", strings.TrimSpace(line))
		}
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

// A second /run, as a reinstall does, must replace the running instance
// rather than be dropped, whatever language schtasks reports status in.
func TestTaskRunReplacesRunningInstance(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "starts.txt")
	w := testTask(t, `C:\Windows\System32\cmd.exe`, fmt.Sprintf(`/c echo start>> "%s" & ping -n 60 127.0.0.1 > nul`, marker))
	starts := func() int {
		b, _ := os.ReadFile(marker)
		return strings.Count(string(b), "start")
	}
	waitFor := func(n int) bool {
		for i := 0; i < 40; i++ {
			if starts() >= n {
				return true
			}
			time.Sleep(250 * time.Millisecond)
		}
		return false
	}
	run := func() {
		if out, err := exec.Command("schtasks", "/run", "/tn", w.taskName).CombinedOutput(); err != nil {
			t.Fatalf("schtasks /run: %s: %v", out, err)
		}
	}

	run()
	if !waitFor(1) {
		t.Skip("task did not run; no interactive session here")
	}
	run()
	if !waitFor(2) {
		t.Fatalf("second /run while the first instance ran: %d start(s), want 2", starts())
	}
	t.Logf("second /run started a new instance: %d starts", starts())
}

func TestSessionUserMatchesProcessUser(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	got := sessionUser()
	t.Logf("session user %q, process user %q", got, u.Username)
	if got != "" && !strings.EqualFold(got, u.Username) {
		t.Fatalf("session user %q differs from process user %q in a plain test run", got, u.Username)
	}
	if got != "" && otherAccount(got) {
		t.Fatalf("otherAccount(%q) = true for this process's own account", got)
	}
	if otherAccount(`NO-SUCH-DOMAIN\no-such-user`) {
		t.Fatal("an account that cannot be looked up must not block install")
	}
}

func TestWindowsLogPathUsesLocalAppData(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\ana\AppData\Local`)
	if got, want := WindowsLogPath(), `C:\Users\ana\AppData\Local\quantifai\quantifai-sync.log`; got != want {
		t.Fatalf("WindowsLogPath() = %q, want %q", got, want)
	}
}
