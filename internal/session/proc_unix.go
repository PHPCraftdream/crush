//go:build !windows

package session

import "syscall"

// isProcessAlive reports whether a process with the given PID is currently
// running. Used to detect orphan locks where the holder crashed without
// releasing — sending signal 0 is the canonical POSIX liveness probe.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// ESRCH = no such process. EPERM means the process exists but we lack
	// permission to signal it — still counts as "alive" for lock purposes.
	return err == syscall.EPERM
}
