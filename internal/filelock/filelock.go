// Package filelock takes and releases exclusive advisory locks on open
// files. On unix it is flock(2); on Windows it is LockFileEx over the
// whole file. Lock blocks until the lock is granted, like LOCK_EX.
package filelock
