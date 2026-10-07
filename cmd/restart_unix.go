//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"syscall"
)

// restartsInPlace reports whether restartInPlace is supported here.
const restartsInPlace = true

// restartInPlace replaces this process with the binary at os.Args[0],
// keeping the PID, the arguments and the environment, as MinIO's
// restartProcess does after an update. launchd and systemd see the same
// process carry on. It returns only on failure.
func restartInPlace() error {
	argv0, err := exec.LookPath(os.Args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(argv0, os.Args, os.Environ())
}
