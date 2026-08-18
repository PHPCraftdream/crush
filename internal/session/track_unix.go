//go:build !windows

package session

import (
	"sync"
	"syscall"
)

// Tree teardown on Unix needs no per-process registration for the ORDINARY
// paths: KillProcess detects process-group leaders via getpgid and kills the
// whole group, and leaderhood is a spawn-time attribute the spawning code
// sets (SysProcAttr.Setpgid), not something retrofittable onto a running pid.
//
// The registry below exists for the one case those paths cannot cover: a
// crush that dies without running its own cleanup. Windows gets that for
// free — its Job Object is kill-on-close, so the OS tears the tree down when
// the process takes the last handle with it. Unix has no equivalent, and
// since CLI children were moved into their own process groups they are no
// longer in the terminal's foreground group either, so a Ctrl-C the terminal
// delivers to that group no longer reaches them.
//
// Measured on Ubuntu 24.04, with the parent catching SIGINT in-process so
// children inherit the default disposition, and liveness decided by Wait()
// returning rather than by kill(pid, 0) — which succeeds for a zombie and
// made two earlier attempts at this measurement read backwards:
//
//	Setpgid=false  child pgid == parent pgid  ->  SIGINT to parent group: child DIED
//	Setpgid=true   child pgid != parent pgid  ->  SIGINT to parent group: child SURVIVED
//
// So the registry records which groups this process is responsible for, and
// KillAllTrackedTrees is the sweep a last-resort signal handler runs before
// exiting.
var (
	trackedGroups   = make(map[int]struct{})
	trackedGroupsMu sync.Mutex
)

// TrackProcessTree records pid as a process group this process must tear
// down if it dies without cleaning up.
//
// Only pids that are their own group leader are recorded. kill(-pid) for a
// non-leader would signal whatever unrelated group happens to carry that
// numeric id — the same hazard KillProcess guards against with its own
// getpgid check. A non-leader pid is not an error: the caller simply did not
// ask for a process group, and the ordinary single-process kill still
// applies to it.
func TrackProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != pid {
		return nil
	}
	trackedGroupsMu.Lock()
	trackedGroups[pid] = struct{}{}
	trackedGroupsMu.Unlock()
	return nil
}

// UntrackProcessTree forgets pid. Safe for a pid that was never tracked,
// which is the common case since non-leaders are not recorded.
func UntrackProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	trackedGroupsMu.Lock()
	delete(trackedGroups, pid)
	trackedGroupsMu.Unlock()
}

// KillAllTrackedTrees SIGKILLs every tracked process group and forgets them.
// It is the Unix stand-in for the kill-on-close Job Object: a last-resort
// sweep for paths where crush is about to die without running its normal
// per-stream cleanup.
//
// Returns how many groups it signalled, which is the only thing a caller can
// usefully log. Errors are deliberately not surfaced: by the time this runs
// there is nothing to do about a group that has already exited, and ESRCH is
// the expected outcome for one that finished on its own.
func KillAllTrackedTrees() int {
	trackedGroupsMu.Lock()
	pids := make([]int, 0, len(trackedGroups))
	for pid := range trackedGroups {
		pids = append(pids, pid)
	}
	trackedGroups = make(map[int]struct{})
	trackedGroupsMu.Unlock()

	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return len(pids)
}
