//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestConsoleHelper is not a test: TestReleaseConsoleClosesConsole runs a
// copy of the test binary on only this function, in a console of its own.
func TestConsoleHelper(t *testing.T) {
	out := os.Getenv("QUANTIFAI_CONSOLE_HELPER")
	if out == "" {
		return
	}
	before, _, _ := getConsoleWindow.Call()
	releaseConsole()
	after, _, _ := getConsoleWindow.Call()
	os.WriteFile(out, []byte(strconv.FormatUint(uint64(before), 10)+" "+strconv.FormatUint(uint64(after), 10)), 0600)
	os.Exit(0)
}

// The supervisor started by the logon task must give up its console, not
// just hide its window: afterwards it has no console at all.
func TestReleaseConsoleClosesConsole(t *testing.T) {
	out := filepath.Join(t.TempDir(), "console")
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsoleHelper$")
	cmd.Env = append(os.Environ(), "QUANTIFAI_CONSOLE_HELPER="+out)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	before, after, _ := strings.Cut(string(b), " ")
	if before == "0" {
		t.Fatalf("helper had no console to start with (%s); the test proves nothing", b)
	}
	if after != "0" {
		t.Fatalf("console window %s still attached after releaseConsole (before: %s)", after, before)
	}
}
