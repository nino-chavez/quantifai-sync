package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeSystemctl puts a systemctl on PATH that counts how many times the
// unit was (re)started, behaving like the real one: "enable --now" starts
// the unit only if it is not running, and restart always starts a new
// process.
func fakeSystemctl(t *testing.T, running bool) (startsPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("systemctl fake is a shell script")
	}
	dir := t.TempDir()
	startsPath = filepath.Join(dir, "starts")
	initial := ""
	if running {
		initial = "start\n"
	}
	if err := os.WriteFile(startsPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
starts="` + startsPath + `"
shift # --user
case "$*" in
daemon-reload) ;;
"enable --now "*) test -s "$starts" || echo start >> "$starts" ;;
"enable "*) ;;
"restart "*) echo start >> "$starts" ;;
*) echo "unexpected: $*" >&2; exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return startsPath
}

func starts(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "start")
}

func TestSystemdInstallRestartsRunningService(t *testing.T) {
	startsPath := fakeSystemctl(t, true)
	sd := &Systemd{unitPath: filepath.Join(t.TempDir(), systemdUnitName), binaryPath: "/x/quantifai-sync"}
	if err := sd.Install(); err != nil {
		t.Fatalf("reinstall over a running service: %v", err)
	}
	if got := starts(t, startsPath); got != 2 {
		t.Errorf("service started %d times, want 2: a reinstall must restart the running service", got)
	}
}

func TestSystemdInstallStartsStoppedService(t *testing.T) {
	startsPath := fakeSystemctl(t, false)
	sd := &Systemd{unitPath: filepath.Join(t.TempDir(), systemdUnitName), binaryPath: "/x/quantifai-sync"}
	if err := sd.Install(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if got := starts(t, startsPath); got != 1 {
		t.Errorf("service started %d times, want 1", got)
	}
}
