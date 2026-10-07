//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// restartsInPlace reports whether restartInPlace is supported here.
const restartsInPlace = true

// restartInPlace replaces this process with the new binary, keeping the
// PID, the arguments and the environment, as MinIO's restartProcess does
// after an update. launchd and systemd see the same process carry on. It
// returns only on failure.
//
// Unlike MinIO it runs os.Executable(), the file the updater located and
// replaced, rather than searching PATH for os.Args[0] again, which could
// find a different copy.
func restartInPlace() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
