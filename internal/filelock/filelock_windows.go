//go:build windows

package filelock

import (
	"os"

	"golang.org/x/sys/windows"
)

// AppendFlags opens a file for appending with a handle Lock accepts.
// LockFileEx needs GENERIC_READ or GENERIC_WRITE, and Go drops
// GENERIC_WRITE from an O_APPEND handle, so O_WRONLY|O_APPEND cannot be
// locked here. O_RDWR keeps GENERIC_READ; writes still append.
const AppendFlags = os.O_RDWR | os.O_APPEND

// allBytes locks the whole file: offset 0 (the zero Overlapped), length
// 2^64-1, as cmd/go's filelock does.
const allBytes = ^uint32(0)

// Lock takes an exclusive lock on all of f, blocking until it is granted.
func Lock(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, allBytes, allBytes, new(windows.Overlapped))
}

// Unlock releases the lock on f.
func Unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, new(windows.Overlapped))
}
