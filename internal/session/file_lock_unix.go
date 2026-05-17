//go:build !windows

package session

import (
	"os"
	"syscall"
)

// lockFileBlocking takes an exclusive lock that BLOCKS until acquired.
// Counterpart to tryLockFile (which is non-blocking via LOCK_NB).
func lockFileBlocking(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
