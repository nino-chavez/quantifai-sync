//go:build windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/quantifai/sync/internal/service"
)

// redirectOutputToLog points stdout and stderr at service.WindowsLogPath,
// appending. Children started afterwards inherit it, so the agent's own
// messages (such as a config problem it is waiting on) land there too.
// The process's standard handles are pointed there as well: the runtime
// writes a panic or fatal error trace to the standard error handle, not to
// os.Stderr, and the supervisor has released its console.
func redirectOutputToLog() error {
	path := service.WindowsLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	os.Stdout, os.Stderr = f, f
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(f.Fd())); err != nil {
		return err
	}
	return windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
}

// killChildrenWithSupervisor puts this process in a job object that kills
// every process in it when the last handle closes. Children inherit the
// job, so when Task Scheduler ends the supervisor (uninstall, reinstall,
// logoff) the agent goes with it instead of running on as an orphan. The
// handle is deliberately never closed; it closes when this process exits.
func killChildrenWithSupervisor() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return err
	}
	return nil
}

// restartLogonTask starts the logon task again so the supervisor runs the
// updated binary: Windows has no exec, and this process's own file was
// renamed aside by the update. The task's StopExisting policy ends this
// instance and starts a new one from the task's path, which now holds the
// update; the agent has already exited.
func restartLogonTask() error {
	cmd := exec.Command("schtasks", "/run", "/tn", service.WindowsTaskName)
	hideChildWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks /run: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// hideChildWindow starts the child without a console window. Every console
// program the supervisor starts needs it: the supervisor has released its
// own console (releaseConsole), so a child without it would open a new,
// visible one.
func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
