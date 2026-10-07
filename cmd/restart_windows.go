//go:build windows

package cmd

import "errors"

// restartsInPlace reports whether restartInPlace is supported here.
// Windows has no exec, and the agent does not yet run as a real Windows
// service that could be restarted, so an update waits for the next start.
const restartsInPlace = false

func restartInPlace() error {
	return errors.New("restart in place is not supported on Windows")
}
