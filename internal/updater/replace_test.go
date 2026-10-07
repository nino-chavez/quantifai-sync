package updater

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestHelperProcess is not a test: TestReplaceExecutableWhileRunning starts a
// copy of the test binary running only this function, so it has a live
// process whose executable it can replace.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("QUANTIFAI_UPDATER_HELPER") != "sleep" {
		return
	}
	os.Stdout.WriteString("ready\n")
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

func TestReplaceExecutableWhileRunning(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "quantifai-sync")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	copyTestFile(t, self, exe)

	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "QUANTIFAI_UPDATER_HELPER=sleep")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	stop := func() {
		if !exited {
			cmd.Process.Kill()
			cmd.Wait()
			exited = true
		}
	}
	defer stop()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper did not start: %q, %v", line, err)
	}

	newBin := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(newBin, []byte("new binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(newBin, exe); err != nil {
		t.Fatalf("replace a running executable: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary" {
		t.Fatalf("executable holds %d bytes, want the new binary", len(got))
	}

	write := func(name, body string) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A second update while the first old process still runs, as when the
	// supervisor keeps running from an earlier binary. On Windows that
	// process holds the .old file, so the update must pick another name.
	if err := replaceExecutable(write("newer", "newer binary"), exe); err != nil {
		t.Fatalf("second replace while the old process runs: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "newer binary" {
		t.Fatalf("second replace left %q", got)
	}

	// Once the old process has exited, the next update clears everything
	// earlier ones left. On Windows, pass the path the old process may
	// report after the swap, which ends in .old.
	stop()
	target := exe
	if runtime.GOOS == "windows" {
		target = exe + ".old"
	}
	if err := replaceExecutable(write("newest", "newest binary"), target); err != nil {
		t.Fatalf("third replace: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "newest binary" {
		t.Fatalf("third replace left %q", got)
	}
	leftovers, _ := filepath.Glob(exe + ".*")
	if len(leftovers) != 0 {
		t.Errorf("left behind after the old process exited: %v", leftovers)
	}
}

func copyTestFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	outF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(outF, in); err != nil {
		t.Fatal(err)
	}
	if err := outF.Close(); err != nil {
		t.Fatal(err)
	}
}
