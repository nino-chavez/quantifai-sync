package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeLaunchctl puts a launchctl on PATH that keeps one label's loaded state
// in a file and logs each call. Like the real one, bootstrap fails while the
// label is still loaded.
func fakeLaunchctl(t *testing.T, loaded bool) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("launchctl fake is a shell script")
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "loaded")
	logPath = filepath.Join(dir, "calls")
	if loaded {
		if err := os.WriteFile(state, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
echo "$1" >> "` + logPath + `"
case "$1" in
print) test -e "` + state + `" ;;
bootout) rm -f "` + state + `" ;;
bootstrap) if test -e "` + state + `"; then echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; fi; touch "` + state + `" ;;
*) exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func calls(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

func TestReloadAgentReplacesLoadedAgent(t *testing.T) {
	logPath := fakeLaunchctl(t, true)
	if err := reloadAgent(plistLabel, filepath.Join(t.TempDir(), "x.plist")); err != nil {
		t.Fatalf("reload a loaded agent: %v", err)
	}
	if got, want := calls(t, logPath), "print bootout print bootstrap"; got != want {
		t.Errorf("launchctl calls = %q, want %q", got, want)
	}
}

func TestReloadAgentStartsUnloadedAgent(t *testing.T) {
	logPath := fakeLaunchctl(t, false)
	if err := reloadAgent(plistLabel, filepath.Join(t.TempDir(), "x.plist")); err != nil {
		t.Fatalf("load an unloaded agent: %v", err)
	}
	if got, want := calls(t, logPath), "print bootstrap"; got != want {
		t.Errorf("launchctl calls = %q, want %q", got, want)
	}
}
