//go:build windows

package cmd

import "errors"

// restartsInPlace reports whether restartInPlace is supported here.
// Windows has no exec. Under the logon task's supervisor the agent exits
// with exitUpdated instead; run by hand, an update waits for the next start.
const restartsInPlace = false

func restartInPlace() error {
	return errors.New("restart in place is not supported on Windows")
}
