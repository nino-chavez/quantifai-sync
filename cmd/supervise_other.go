//go:build !windows

package cmd

import "os/exec"

// killChildrenWithSupervisor is a no-op: only Windows needs the supervisor.
func killChildrenWithSupervisor() error { return nil }

// redirectOutputToLog is a no-op: launchd and systemd capture output.
func redirectOutputToLog() error { return nil }

// hideChildWindow is a no-op: there is no console window to hide.
func hideChildWindow(*exec.Cmd) {}
