//go:build !windows

package cmd

// hideConsole is a no-op: only the Windows logon task opens a console window.
func hideConsole() {}

// releaseConsole is a no-op, for the same reason.
func releaseConsole() {}
