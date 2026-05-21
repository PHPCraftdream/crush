//go:build windows

package session

import "golang.org/x/sys/windows"

// isProcessAlive reports whether a process with the given PID is currently
// running. Used to detect orphan locks where the holder crashed without
// releasing — on Windows we open the process with PROCESS_QUERY_LIMITED_
// INFORMATION (cheap, doesn't require admin), then ask for its exit code.
// STILL_ACTIVE (259) means alive; anything else means it has exited.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
