//go:build !windows

package session

import (
	"os"
	"syscall"
)

// tryLockFile takes an exclusive non-blocking advisory lock using
// flock(2). Returns an error (non-nil, typically EWOULDBLOCK) if
// another process already holds the lock.
func tryLockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlockFile releases the lock taken by tryLockFile.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
