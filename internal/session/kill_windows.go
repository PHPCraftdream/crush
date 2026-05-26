//go:build windows

package session

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows"
)

// KillProcess forcibly terminates the process tree rooted at pid.
//
// On Windows os.Process.Kill() goes through OpenProcess(PROCESS_TERMINATE)
// which can fail with "Access is denied" for processes spawned under a
// different shell (Git Bash / MSYS, elevated console host, etc.). We
// prefer two more reliable paths:
//
//  1. taskkill /F /T /PID <pid> — kills the process plus every child it
//     spawned. This is what crush sessions kill actually wants because
//     a stuck crush.exe usually has a claude.cmd / node.exe descendant
//     still holding its stdin pipe.
//  2. As a fallback (taskkill not on PATH) OpenProcess + TerminateProcess
//     via golang.org/x/sys/windows.
//
// Returns nil if the process is already gone. Caller is expected to poll
// IsProcessAlive afterwards.
func KillProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("KillProcess: invalid pid %d", pid)
	}
	if !isProcessAlive(pid) {
		return nil
	}
	if path, lookErr := exec.LookPath("taskkill"); lookErr == nil {
		out, err := exec.Command(path, "/F", "/T", "/PID", fmt.Sprintf("%d", pid)).CombinedOutput()
		if err == nil {
			return nil
		}
		if !isProcessAlive(pid) {
			return nil
		}
		_ = out
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if !isProcessAlive(pid) {
			return nil
		}
		return fmt.Errorf("KillProcess: OpenProcess %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		if !isProcessAlive(pid) {
			return nil
		}
		return fmt.Errorf("KillProcess: TerminateProcess %d: %w", pid, err)
	}
	return nil
}
