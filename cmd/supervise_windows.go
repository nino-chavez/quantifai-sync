//go:build windows

package cmd

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// killChildrenWithSupervisor puts this process in a job object that kills
// every process in it when the last handle closes. Children inherit the
// job, so when Task Scheduler ends the supervisor (uninstall, reinstall,
// logoff) the agent goes with it instead of running on as an orphan. The
// handle is deliberately never closed; it closes when this process exits.
func killChildrenWithSupervisor() error {
	return nil // TEMPORARY: no job object, to measure orphans on CI
}

// hideChildWindow starts the child without a console window.
func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
