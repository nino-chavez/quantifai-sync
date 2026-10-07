//go:build windows

package cmd

import "errors"

// restartsInPlace reports whether restartInPlace is supported here.
// Windows has no exec, so an update waits for the logon task's next start.
const restartsInPlace = false

func restartInPlace() error {
	return errors.New("restart in place is not supported on Windows")
}
