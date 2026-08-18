//go:build !windows

package session

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The Unix registry behind KillAllTrackedTrees — the last-resort sweep that
// stands in for Windows' kill-on-close Job Object when crush is about to die
// without running its own cleanup.
//
// The property that matters most here is the NEGATIVE one: a pid that is not
// its own group leader must never be recorded. Sweeping it would mean
// kill(-pid) against whatever unrelated process group happens to carry that
// numeric id — including, on an unlucky day, crush's own.

// spawnGroupChild starts a long-lived child, optionally as its own process
// group leader, and returns it with a reaper already waiting.
func spawnGroupChild(t *testing.T, ownGroup bool) (*exec.Cmd, <-chan error) {
	t.Helper()
	// Deliberately not a round duration: other packages' tests run
	// concurrently under the pre-push hook and also sleep, and a cleanup
	// that matches on command line must not catch theirs.
	cmd := exec.Command("sleep", "43.17")
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	require.NoError(t, cmd.Start())

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
		}
	})
	// Let the exec complete so Getpgid sees the final group.
	time.Sleep(80 * time.Millisecond)
	return cmd, waited
}

func TestTrackProcessTree_RecordsOnlyGroupLeaders(t *testing.T) {
	leader, _ := spawnGroupChild(t, true)
	follower, _ := spawnGroupChild(t, false)

	require.NoError(t, TrackProcessTree(leader.Process.Pid))
	require.NoError(t, TrackProcessTree(follower.Process.Pid))
	t.Cleanup(func() { KillAllTrackedTrees() })

	trackedGroupsMu.Lock()
	_, leaderTracked := trackedGroups[leader.Process.Pid]
	_, followerTracked := trackedGroups[follower.Process.Pid]
	trackedGroupsMu.Unlock()

	require.True(t, leaderTracked, "a group leader must be recorded — it is what the sweep exists for")
	require.False(t, followerTracked,
		"a non-leader must NOT be recorded: sweeping it would kill(-pid) some unrelated "+
			"process group that happens to carry that numeric id")
}

func TestKillAllTrackedTrees_KillsTheGroupAndForgetsIt(t *testing.T) {
	leader, waited := spawnGroupChild(t, true)
	require.NoError(t, TrackProcessTree(leader.Process.Pid))

	n := KillAllTrackedTrees()
	require.Equal(t, 1, n, "exactly the one tracked group must be swept")

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the tracked process group survived KillAllTrackedTrees")
	}

	trackedGroupsMu.Lock()
	remaining := len(trackedGroups)
	trackedGroupsMu.Unlock()
	require.Zero(t, remaining, "the sweep must forget what it swept, or a later sweep repeats it")

	require.Zero(t, KillAllTrackedTrees(), "a second sweep has nothing left to do")
}

func TestUntrackProcessTree_RemovesFromTheSweep(t *testing.T) {
	leader, _ := spawnGroupChild(t, true)
	require.NoError(t, TrackProcessTree(leader.Process.Pid))

	UntrackProcessTree(leader.Process.Pid)
	require.Zero(t, KillAllTrackedTrees(), "an untracked group must not be swept")

	// And the process is genuinely still alive — the point of untracking.
	require.NoError(t, syscall.Kill(leader.Process.Pid, 0),
		"untracking must not kill anything by itself")
}
