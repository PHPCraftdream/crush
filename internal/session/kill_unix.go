//go:build !windows

package session

import (
	"fmt"
	"os"
	"syscall"
)

// KillProcess forcibly terminates the process with the given PID.
// POSIX: SIGKILL via os.Process.Signal. Returns nil if the process is
// already gone (ESRCH). Caller is expected to poll IsProcessAlive
// afterwards to wait for reap.
func KillProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("KillProcess: invalid pid %d", pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("KillProcess: find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		if err == os.ErrProcessDone {
			return nil
		}
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("KillProcess: SIGKILL %d: %w", pid, err)
	}
	return nil
}
