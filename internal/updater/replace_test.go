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
	os.WriteFile(newBin, []byte("new binary"), 0755)
	if err := replaceExecutable(newBin, exe); err != nil {
		t.Fatalf("replace a running executable: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary" {
		t.Fatalf("executable holds %d bytes, want the new binary", len(got))
	}

	// Once the old process has exited, the next update replaces again and
	// clears what the first one left behind.
	stop()
	newer := filepath.Join(t.TempDir(), "newer")
	os.WriteFile(newer, []byte("newer binary"), 0755)
	if err := replaceExecutable(newer, exe); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "newer binary" {
		t.Fatalf("second replace left %q", got)
	}
	for _, leftover := range []string{exe + ".old", exe + ".new"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s left behind after the old process exited", filepath.Base(leftover))
		}
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
