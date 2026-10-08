//go:build windows

package cmd

import "golang.org/x/sys/windows"

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	freeConsole      = kernel32.NewProc("FreeConsole")
	getConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	showWindow       = windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow")
)

// releaseConsole detaches the supervisor from the console the logon task
// opens for it. "A console is closed when the last process attached to it
// terminates or calls FreeConsole" (FreeConsole docs), so its window goes
// whichever program hosts it. Hiding the window is not enough: in a
// pseudoconsole session, as when Windows Terminal hosts the console,
// GetConsoleWindow returns a window that "is not displayed locally", and
// the visible one stays. The console still appears briefly at logon.
//
// Every console program the supervisor starts afterwards must use
// hideChildWindow; without a console to inherit, each would get a new,
// visible one.
func releaseConsole() {
	if freeConsole.Find() == nil {
		freeConsole.Call()
	}
}

// hideConsole hides the console window of an agent run with --no-console
// but no supervisor. It keeps its console, so the programs it runs (git)
// do not each open a window. Best-effort: see releaseConsole.
func hideConsole() {
	if getConsoleWindow.Find() != nil || showWindow.Find() != nil {
		return
	}
	if hwnd, _, _ := getConsoleWindow.Call(); hwnd != 0 {
		showWindow.Call(hwnd, windows.SW_HIDE)
	}
}
