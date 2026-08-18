//go:build windows

package session

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// spawnLiveVictim starts a live, untracked process that plays the role
// of whatever unrelated process the OS later assigned a pid that a
// leaked trackedJobs entry still keys. Returns the pid and registers
// the cleanup kill.
func spawnLiveVictim(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "cmd.exe", "/c", "ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn victim process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	require.True(t, isProcessAlive(cmd.Process.Pid), "victim must be alive after Start")
	return cmd.Process.Pid
}

// plantStaleTrackedEntry fabricates the abandoned-stream leak directly:
// a real (empty) Job Object handle registered under pid with a creation
// time that matches no live process — exactly the map state an
// un-iterated cliprovider stream leaves behind on Windows once the OS
// has recycled its child's pid (reachable there because the PTY branch
// reaps the child eagerly, closing the process handle that would
// otherwise pin the pid).
func plantStaleTrackedEntry(t *testing.T, pid int) {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	require.NoError(t, err, "CreateJobObject")
	trackedJobsMu.Lock()
	trackedJobs[pid] = trackedJob{handle: job, createdAt: windows.Filetime{}}
	trackedJobsMu.Unlock()
	t.Cleanup(func() { UntrackProcessTree(pid) })
}

func trackedEntryExists(pid int) bool {
	trackedJobsMu.Lock()
	defer trackedJobsMu.Unlock()
	_, ok := trackedJobs[pid]
	return ok
}

// TestTerminateTrackedJob_ReusedPidEntryReturnsFalse pins the core of
// the pid-reuse guard in isolation: a stale entry (dead tree, recycled
// pid now held by an unrelated live process) must NOT report successful
// termination. TerminateJobObject on the empty stale job succeeds
// vacuously — observed directly while designing this fix, nil error for
// both an empty job and a job whose only member had exited — so without
// the creation-time identity check this function returns true while the
// live pid-holder is untouched, and any caller trusting that true (the
// pre-restructure KillProcess short-circuit did exactly that) both
// fakes a kill and skips the taskkill fallback that would have worked.
//
// REVERT CHECK: removing the pidIsSameProcess call from
// terminateTrackedJob makes this fail on "returned false".
func TestTerminateTrackedJob_ReusedPidEntryReturnsFalse(t *testing.T) {
	pid := spawnLiveVictim(t)
	plantStaleTrackedEntry(t, pid)

	got := terminateTrackedJob(pid)
	require.False(t, got, "stale entry must not report termination success — the live pid-holder is a different process")
	require.True(t, isProcessAlive(pid), "the reused-pid process must be untouched by the stale job path")
	require.False(t, trackedEntryExists(pid), "stale entry must still be consumed (removed and closed)")
}

// TestKillProcess_StaleTrackedEntryStillKillsReusedPid is the
// end-to-end form of the same defect: with a stale entry planted for a
// live pid, KillProcess must actually kill that process and return nil
// only because it is really dead — never the pre-fix shape where
// TerminateJobObject's vacuous success on the empty job returned nil
// with the process alive and the taskkill fallback disabled.
//
// REVERT CHECK: restoring the original 68f9c65f shape —
// `if terminateTrackedJob(pid) { return nil }` with no identity check —
// makes this fail on "survived": the vacuous TerminateJobObject success
// returns nil with the process alive and taskkill never reached. (The
// short-circuit alone, with the identity check kept, no longer reddens
// this test: the mismatch makes terminateTrackedJob return false and
// the pid-based paths still kill; the identity check in isolation is
// pinned by TestTerminateTrackedJob_ReusedPidEntryReturnsFalse.)
func TestKillProcess_StaleTrackedEntryStillKillsReusedPid(t *testing.T) {
	pid := spawnLiveVictim(t)
	plantStaleTrackedEntry(t, pid)

	require.NoError(t, KillProcess(pid), "KillProcess on a live reused pid")
	require.False(t, trackedEntryExists(pid), "entry must be consumed by the kill")
	// Kill is asynchronous; poll briefly like the callers do.
	for i := 0; i < 50 && isProcessAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	require.False(t, isProcessAlive(pid), "KillProcess returned nil but the reused-pid process survived — the stale job faked success and disabled the fallback")
}

// TestKillProcess_TrackedLiveEntryKillsTree guards the normal path
// through the identity check: a correctly tracked live entry must still
// terminate (and consume) its job, so the pid-reuse guard cannot start
// rejecting legitimate kills.
func TestKillProcess_TrackedLiveEntryKillsTree(t *testing.T) {
	pid := spawnLiveVictim(t)
	require.NoError(t, TrackProcessTree(pid), "TrackProcessTree")
	require.True(t, trackedEntryExists(pid), "entry registered")

	require.NoError(t, KillProcess(pid), "KillProcess on tracked live pid")
	for i := 0; i < 50 && isProcessAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	require.False(t, isProcessAlive(pid), "tracked live pid must be dead after KillProcess")
	require.False(t, trackedEntryExists(pid), "entry must be consumed by the kill")
}
