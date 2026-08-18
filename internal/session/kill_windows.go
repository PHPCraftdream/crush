//go:build windows

package session

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/charmbracelet/crush/internal/platform"
	"golang.org/x/sys/windows"
)

// KillProcess forcibly terminates the process tree rooted at pid.
//
// Tracked pids (registered via TrackProcessTree at spawn time) are killed
// by BOTH mechanisms below, deliberately not short-circuited: taskkill
// /F /T first, then the Job Object. The two mechanisms have disjoint
// blind spots — taskkill /T walks the PPID chain and so cannot see past
// the MSYS2/Git-Bash transient intermediary parent (job membership
// reaches those), while the job cannot see a process that escaped in the
// Start→AssignProcessToJobObject micro-gap and IS PPID-reachable when
// crush itself does not run under MSYS (taskkill reaches those).
// Returning early on a successful TerminateJobObject — the shape this
// function had after 68f9c65f — silently dropped that second case: the
// job terminated "successfully", KillProcess reported nil, and the
// escapee survived with its fallback explicitly disabled. taskkill runs
// BEFORE the job pass because /T needs the root alive to walk from and
// the job termination kills the root. Cost: one taskkill spawn per kill
// of a tracked child; kill paths only run on stream cancellation, never
// per-stream, so this is not hot.
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
		// Already gone. Still consume any tracked entry so it cannot
		// later collide with a recycled pid (see terminateTrackedJob);
		// closing its KILL_ON_JOB_CLOSE handle also tears down any
		// straggler descendant still inside the job.
		terminateTrackedJob(pid)
		return nil
	}
	if path, lookErr := exec.LookPath("taskkill"); lookErr == nil {
		cmd := platform.Command(context.Background(), path, "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
		_, _ = cmd.CombinedOutput()
		// taskkill errors are not surfaced directly: the pid-based
		// aliveness checks below decide the outcome, which also covers
		// taskkill /T missing an MSYS-broken chain it cannot walk.
	}
	// Job pass: one TerminateJobObject takes down the tracked tree,
	// MSYS-broken chains included. Identity-checked inside, so a stale
	// entry whose pid the OS has since recycled is closed without
	// faking success.
	terminateTrackedJob(pid)
	if !isProcessAlive(pid) {
		return nil
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
