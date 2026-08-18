//go:build !windows

package session

import (
	"fmt"
	"os"
	"syscall"
)

// KillProcess forcibly terminates the process with the given PID and,
// when that pid is a process-group leader, every other process in its
// group — the direct child plus any grandchildren it spawned (a CLI
// provider's claude.cmd → cmd.exe → node.exe chain, MCP servers the CLI
// itself starts, …) — so they die with the target instead of being
// orphaned and running to completion. See
// internal/agent/tools/mcp/process_unix.go for the same leak shape
// fixed on stdio MCP servers.
//
// The group kill is attempted ONLY after syscall.Getpgid confirms pid
// is the leader of group pid. kill(-pid) for a non-leader pid would
// signal whatever unrelated process group happens to carry that numeric
// id (a lingering group whose leader died, or — worse — our own), so
// without that confirmation we fall back to the single-process SIGKILL
// below and leave every group untouched. Callers that want tree-kill
// semantics must spawn the child with SysProcAttr.Setpgid = true;
// foreign pids (the lock holder `crush sessions kill` targets, for
// example) keep the old single-process behavior unless they already
// happen to be leaders.
//
// Returns nil if the process is already gone (ESRCH). Caller is
// expected to poll IsProcessAlive afterwards to wait for reap.
func KillProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("KillProcess: invalid pid %d", pid)
	}
	pgid, pgidErr := syscall.Getpgid(pid)
	if pgidErr == nil && pgid == pid {
		killErr := syscall.Kill(-pid, syscall.SIGKILL)
		switch {
		case killErr == nil:
			return nil
		case isErrno(killErr, syscall.ESRCH):
			// The whole group — leader included — is already gone.
			return nil
		case isErrno(killErr, syscall.EPERM):
			// Some member of the group is not ours to signal (foreign
			// uid). The direct kill below may still succeed for a
			// child we own; fall through to it.
		default:
			return fmt.Errorf("KillProcess: killpg %d: %w", pid, killErr)
		}
	} else if isErrno(pgidErr, syscall.ESRCH) {
		return nil
	}
	// Not a group leader (or the group kill was not permitted): a
	// single SIGKILL. Never signal -pid without the leader check
	// above — that would address some other process's group.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("KillProcess: find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		if err == os.ErrProcessDone {
			return nil
		}
		if isErrno(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("KillProcess: SIGKILL %d: %w", pid, err)
	}
	return nil
}

// isErrno reports whether err is one of the raw syscall.Errno values in
// want. Errors returned by os.Process signal paths arrive unwrapped, so
// a direct type assertion matches the style previously used here.
func isErrno(err error, want ...syscall.Errno) bool {
	errno, ok := err.(syscall.Errno)
	if !ok {
		return false
	}
	for _, w := range want {
		if errno == w {
			return true
		}
	}
	return false
}
