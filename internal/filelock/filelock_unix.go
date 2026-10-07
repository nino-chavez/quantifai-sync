//go:build !windows

package filelock

import (
	"os"
	"syscall"
)

// AppendFlags opens a file for appending with a handle Lock accepts.
const AppendFlags = os.O_WRONLY | os.O_APPEND

// Lock takes an exclusive flock on f, blocking until it is granted.
func Lock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// Unlock releases the flock on f.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
