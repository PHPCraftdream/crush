//go:build !windows

package session

import (
	"errors"
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

// isLockContentionError reports whether err from tryLockFile means
// "another process holds this lock right now" (EWOULDBLOCK/EAGAIN from
// flock(2) with LOCK_NB) as opposed to some other, unrelated failure
// (permission denied, stale NFS handle, etc). Callers must not treat
// the latter as "busy" — see acquireSessionLockFile.
func isLockContentionError(err error) bool {
	// On Linux/BSD/macOS, EWOULDBLOCK and EAGAIN are the same errno value;
	// checking both keeps this correct even where they diverge.
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
