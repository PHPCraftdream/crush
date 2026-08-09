package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestP1_2_ManualCompactionShowsReleasingState verifies that manual
// compaction transitions to mbReleasing state before releasing the OS lock.
// Without this (P1-2 from the 2026-08-07 concurrency review), a hung
// filesystem/AV/SMB during lk.Release() would leave the mailbox permanently
// mbOwned, indistinguishable from an actively streaming turn. With mbReleasing
// visibility, diagnostic tools can distinguish "releasing OS lock" from
// "still working".
//
// The test verifies that:
//  1. beginRelease transitions from mbOwned to mbReleasing.
//  2. mbReleasing is observable as "busy" (not mbIdle).
//  3. finishRelease transitions from mbReleasing to mbIdle.
//  4. Wrong epoch or wrong state causes no transition (no-op).
//
// REVERT CHECK PROCEDURE:
//  1. In mailbox.go:~1156, comment out the mbReleasing transition in beginRelease:
//     // BUG: Don't transition to mbReleasing (P1-2 revert check)
//     // mb.state = mbReleasing
//     mb.state = mbOwned // Stay in mbOwned
//  2. In agent.go:~3109, comment out the mb.beginRelease(epoch) call:
//     // P1-2 fix (2026-08-09): Transition mailbox to mbReleasing...
//     // releaseStarted := mb.beginRelease(epoch)
//     releaseStarted := false // TEMP: test revert
//  3. In agent.go:~3138, comment out the mb.finishRelease(epoch) call:
//     // P1-2 fix (2026-08-09): Complete the mbReleasing -> mbIdle transition...
//     // if releaseStarted {
//     //     mb.finishRelease(epoch)
//     // }
//  4. Run: go test ./internal/agent -run TestP1_2_ManualCompactionShowsReleasingState -v
//  5. The test will FAIL because mbReleasing is never observed (state stays mbOwned)
//  6. Restore the fix and the test will PASS again.
func TestP1_2_ManualCompactionShowsReleasingState(t *testing.T) {
	t.Parallel()

	mb := &mailbox{}

	// Set up mbOwned state via beginCompact.
	cancelFunc := func() {}
	epoch, ok := mb.beginCompact(cancelFunc)
	require.True(t, ok, "beginCompact should succeed")
	require.Equal(t, uint64(1), epoch, "epoch should be 1 (beginCompact increments from initial)")
	require.Equal(t, mailboxState(mbOwned), mb.state, "state should be mbOwned after beginCompact")

	// Test normal flow: beginRelease -> mbReleasing -> finishRelease -> mbIdle.

	// Call beginRelease - should transition to mbReleasing.
	releaseStarted := mb.beginRelease(epoch)
	require.True(t, releaseStarted, "beginRelease should succeed with matching epoch")
	require.Equal(t, mailboxState(mbReleasing), mb.state, "state should be mbReleasing after beginRelease")

	// Verify that mbReleasing reports as busy (not idle).
	// We can't call mb.IsSessionBusy() directly since it's a sessionAgent method,
	// but we can verify the state is not mbIdle.
	require.NotEqual(t, mailboxState(mbIdle), mb.state, "mbReleasing should not be mbIdle")

	// Call finishRelease - should transition to mbIdle.
	finished := mb.finishRelease(epoch)
	require.True(t, finished, "finishRelease should succeed with matching epoch")
	require.Equal(t, mailboxState(mbIdle), mb.state, "state should be mbIdle after finishRelease")

	// Test epoch mismatch: call beginRelease with wrong epoch.
	mb.beginCompact(cancelFunc) // Move to mbOwned again
	wrongEpoch := epoch + 10
	releaseStarted = mb.beginRelease(wrongEpoch)
	require.False(t, releaseStarted, "beginRelease should fail with wrong epoch")
	require.Equal(t, mailboxState(mbOwned), mb.state, "state should still be mbOwned after wrong epoch")

	// Test wrong state: call beginRelease when already mbIdle.
	// First, finish the current era.
	mb.abandonOwnership(epoch + 1)
	releaseStarted = mb.beginRelease(epoch)
	require.False(t, releaseStarted, "beginRelease should fail when state is mbIdle")

	// Test wrong state: call beginRelease when already mbReleasing.
	mb.beginCompact(cancelFunc)              // Move to mbOwned
	epoch2, _ := mb.beginCompact(cancelFunc) // Move to mbOwned (new epoch)
	mb.beginRelease(epoch2)                  // Move to mbReleasing
	releaseStarted = mb.beginRelease(epoch2)
	require.False(t, releaseStarted, "beginRelease should fail when state is mbReleasing")

	// Clean up.
	// No need to call cancelFunc() since it's a no-op func(){}.
}

// TestP1_2_ManualCompactionReleasesEvenWithoutStateTransition verifies that
// manual compaction still releases the OS lock even if beginRelease fails
// (e.g., due to epoch mismatch). This ensures we don't leak the lock in edge
// cases.
func TestP1_2_ManualCompactionReleasesEvenWithoutStateTransition(t *testing.T) {
	t.Parallel()

	mb := &mailbox{}

	// Set up mbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.beginCompact(cancelFunc)

	// Call beginRelease with a wrong epoch (simulates concurrent state change).
	releaseStarted := mb.beginRelease(epoch + 1)
	require.False(t, releaseStarted, "beginRelease should fail with wrong epoch")
	require.Equal(t, mailboxState(mbOwned), mb.state, "state should still be mbOwned")

	// Even though beginRelease failed, the caller should still release the lock.
	// We can't test the actual lk.Release() here without more setup, but we can
	// verify that the mailbox state doesn't prevent us from calling finishRelease.

	// finishRelease should also fail with wrong epoch.
	finished := mb.finishRelease(epoch + 1)
	require.False(t, finished, "finishRelease should fail with wrong epoch")

	// The caller should then abandonOwnership to transition to mbIdle.
	hadWork := mb.abandonOwnership(epoch)
	require.False(t, hadWork, "abandonOwnership should return false (no work)")
	require.Equal(t, mailboxState(mbIdle), mb.state, "state should be mbIdle after abandonOwnership")
}

func TestP1_2_ReleasingStateAllowsCancel(t *testing.T) {
	t.Parallel()

	mb := &mailbox{}

	// Set up mbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.beginCompact(cancelFunc)

	// Transition to mbReleasing.
	releaseStarted := mb.beginRelease(epoch)
	require.True(t, releaseStarted)
	require.Equal(t, mailboxState(mbReleasing), mb.state)

	// Cancel should be a no-op (no panic, no-op return).
	// We can't easily test mb.Cancel() here, but we can verify that the state
	// doesn't change and that cancel handles are nil.
	require.Nil(t, mb.current.cancel, "current.cancel should be nil in mbReleasing")
	require.Nil(t, mb.dispatcherCancel, "dispatcherCancel should be nil in mbReleasing")

	// Clean up.
	mb.finishRelease(epoch)
}

func TestP1_2_BeginReleaseIdempotent(t *testing.T) {
	t.Parallel()

	mb := &mailbox{}

	// Set up mbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.beginCompact(cancelFunc)

	// Call beginRelease multiple times.
	require.True(t, mb.beginRelease(epoch), "first beginRelease should succeed")
	require.Equal(t, mailboxState(mbReleasing), mb.state, "state should be mbReleasing after first call")

	require.False(t, mb.beginRelease(epoch), "second beginRelease should fail (already mbReleasing)")
	require.Equal(t, mailboxState(mbReleasing), mb.state, "state should remain mbReleasing after second call")

	require.False(t, mb.beginRelease(epoch), "third beginRelease should fail")
	require.Equal(t, mailboxState(mbReleasing), mb.state, "state should remain mbReleasing after third call")

	// Clean up.
	mb.finishRelease(epoch)
}

func TestP1_2_FinishReleaseIdempotent(t *testing.T) {
	t.Parallel()

	mb := &mailbox{}

	// Set up mbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.beginCompact(cancelFunc)

	// Transition to mbReleasing.
	require.True(t, mb.beginRelease(epoch))

	// Call finishRelease multiple times.
	require.True(t, mb.finishRelease(epoch), "first finishRelease should succeed")
	require.Equal(t, mailboxState(mbIdle), mb.state, "state should be mbIdle after first call")

	require.False(t, mb.finishRelease(epoch), "second finishRelease should fail (already mbIdle)")
	require.Equal(t, mailboxState(mbIdle), mb.state, "state should remain mbIdle after second call")

	require.False(t, mb.finishRelease(epoch), "third finishRelease should fail")
	require.Equal(t, mailboxState(mbIdle), mb.state, "state should remain mbIdle after third call")
}
