//go:build windows

package cmd

import "golang.org/x/sys/windows"

// hideConsole hides the console window the logon task opens for the agent,
// as Syncthing's --no-console does. Best-effort: when Windows Terminal hosts
// the console, the window it returns may not be the visible one.
func hideConsole() {
	getConsoleWindow := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	showWindow := windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow")
	if getConsoleWindow.Find() != nil || showWindow.Find() != nil {
		return
	}
	if hwnd, _, _ := getConsoleWindow.Call(); hwnd != 0 {
		showWindow.Call(hwnd, windows.SW_HIDE)
	}
}
