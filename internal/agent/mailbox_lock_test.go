package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestMailbox_DrainOrReleaseFinal_OSLockReleasedBeforeStateFlipsIdle is the
// deterministic regression test for round 11 review, HIGH-1.
//
// Before the fix, the end-of-turn drain was two SEPARATE steps with no
// shared critical section: (1) mailbox.drainOrRelease flips mb.state to
// mbIdle under mb.mu and returns, then (2) — only much later, after
// runTurn's own deferred wg.Wait() (title generation) and the rest of the
// call stack unwound back up into Run() — Run's own `defer lk.Release()`
// finally released the OS-level session.SessionLock. Any same-process
// observer of IsSessionBusy(sessionID) (a plain `mb.state != mbIdle` read
// under mb.mu) could see "not busy" strictly BEFORE step 2 ran, legitimately
// try to become the new owner, and hit a spurious SessionLockBusyError from
// its OWN PROCESS's prior, not-yet-unwound turn — silently dropping the
// message (tryReserveSession's owner-branch does not requeue on that error).
//
// This test proves the fix removes that two-step shape entirely by
// reconstructing BOTH shapes against the exact same real OS-level
// session.SessionLock and observing the difference directly:
//
//   - "old shape": call mb.drainOrRelease(epoch) (the mailbox-only primitive,
//     still present for direct unit testing) and, WITHOUT holding mb.mu,
//     probe the real OS lock before calling lk.Release() separately/later —
//     proving the old two-step API allows exactly the observable gap the
//     bug report describes: state already idle, lock still held.
//   - "new shape": call mb.drainOrReleaseFinal(epoch, ...) with the SAME
//     lk.Release as its release callback — proving that by the time it
//     returns hasNext=false, the OS lock is unconditionally already free,
//     because release() runs inside the same mb.mu critical section that
//     flips the state, with no separate later step for a same-process
//     caller to race against.
func TestMailbox_DrainOrReleaseFinal_OSLockReleasedBeforeStateFlipsIdle(t *testing.T) {
	dataDir := t.TempDir()

	t.Run("old two-step shape reproduces the gap", func(t *testing.T) {
		const sessionID = "high-1-lock-test-old-shape"
		lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, err)

		mb := &mailbox{
			state:            mbOwned,
			epoch:            1,
			dispatcherCancel: func() {},
			current:          generation{id: 1, cancel: func() {}},
		}

		// Step 1 (old shape): mailbox-only release. Nothing about this call
		// touches the OS lock at all.
		_, hasNext := mb.drainOrRelease(1)
		require.False(t, hasNext)
		require.Equal(t, mbIdle, mb.state, "mailbox reports idle immediately after step 1")

		// The observable bug: state is idle, but the OS lock this "turn" was
		// holding has NOT been released yet (step 2 hasn't run). A
		// same-process caller relying on IsSessionBusy==false would wrongly
		// believe the lock is free here.
		_, lockErr := session.TryAcquireSessionLock(dataDir, sessionID)
		require.Error(t, lockErr, "reproduction: with the old two-step shape, the OS lock is still held even though "+
			"the mailbox already reports idle — this IS the HIGH-1 gap")

		// Step 2 (old shape, "later"): release finally happens.
		require.NoError(t, lk.Release())
		lk2, err := session.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, err)
		require.NoError(t, lk2.Release())
	})

	t.Run("new atomic shape has no gap", func(t *testing.T) {
		const sessionID = "high-1-lock-test-new-shape"
		lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, err)

		mb := &mailbox{
			state:            mbOwned,
			epoch:            1,
			dispatcherCancel: func() {},
			current:          generation{id: 1, cancel: func() {}},
		}

		_, hasNext, releaseErr := mb.drainOrReleaseFinal(1, nil, nil, lk.Release)
		require.False(t, hasNext)
		require.NoError(t, releaseErr)
		require.Equal(t, mbIdle, mb.state)

		// THE core HIGH-1 assertion: the instant drainOrReleaseFinal reports
		// idle, the OS lock MUST already be free — no separate later step
		// remains for a same-process caller to race against.
		lk2, lockErr := session.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, lockErr, "the OS lock must be acquirable immediately once drainOrReleaseFinal reports "+
			"idle — IsSessionBusy()==false must mean the OS lock is really free, not just that the in-memory "+
			"state flipped first (round 11 review, HIGH-1)")
		require.NoError(t, lk2.Release())
	})
}

// TestMailbox_DrainOrReleaseFinal_MuHeldForWholeOfReleaseCallback is the
// structural companion proving the invariant directly, independent of any
// particular internal statement ordering inside drainOrReleaseFinal: mb.mu
// must still be held for the ENTIRE duration of the release() callback, not
// just up to the point release() is invoked. This is what makes the "new
// atomic shape" half of the sibling test's guarantee robust — if
// drainOrReleaseFinal ever released mb.mu before release() returned (e.g. a
// future refactor that unlocks early to "avoid holding mb.mu during I/O"),
// this test — unlike a purely sequential post-condition check — would catch
// it: it makes release() itself pause, and from a SEPARATE goroutine
// attempts mb.submit(), which also needs mb.mu. If mb.mu were released
// early, submit() would complete (observe mbIdle prematurely, or at least
// stop blocking) while release() is still paused; this test proves it
// cannot.
func TestMailbox_DrainOrReleaseFinal_MuHeldForWholeOfReleaseCallback(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	releaseEntered := make(chan struct{})
	releaseMayReturn := make(chan struct{})
	release := func() error {
		close(releaseEntered)
		<-releaseMayReturn
		return nil
	}

	type drainResult struct {
		hasNext    bool
		releaseErr error
	}
	drainDone := make(chan drainResult, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, hasNext, releaseErr := mb.drainOrReleaseFinal(1, nil, nil, release)
		drainDone <- drainResult{hasNext: hasNext, releaseErr: releaseErr}
	}()

	select {
	case <-releaseEntered:
	case <-drainDone:
		t.Fatal("drainOrReleaseFinal returned before release() was even entered")
	}

	// release() is now paused, still inside drainOrReleaseFinal's call.
	// Fire a concurrent submit() from another goroutine: if mb.mu is still
	// held (the required invariant), this call blocks on mb.mu.Lock() and
	// cannot report a result until release() returns and drainOrReleaseFinal
	// releases mb.mu itself.
	submitDone := make(chan struct {
		becomeOwner bool
		epoch       uint64
	}, 1)
	go func() {
		becomeOwner, epoch := mb.submit(SessionAgentCall{SessionID: "s1", Prompt: "concurrent"}, func() {})
		submitDone <- struct {
			becomeOwner bool
			epoch       uint64
		}{becomeOwner, epoch}
	}()

	select {
	case <-submitDone:
		t.Fatal("submit() completed while release() was still paused — mb.mu was NOT held for the whole of the " +
			"release callback, reopening the exact HIGH-1 gap (a same-process caller could observe/act on a " +
			"mailbox state that does not yet reflect the OS lock's real status)")
	case <-time.After(100 * time.Millisecond):
		// Expected: submit() is blocked on mb.mu, proving mb.mu is still
		// held while release() is paused.
	}

	close(releaseMayReturn)
	wg.Wait()

	result := <-drainDone
	require.False(t, result.hasNext)
	require.NoError(t, result.releaseErr)
	// NOTE: mb.state is deliberately NOT asserted here — the moment
	// drainOrReleaseFinal releases mb.mu, the blocked submit() goroutine
	// races to reacquire it and may flip state back to mbOwned before this
	// goroutine gets a chance to read it. That race is expected and correct
	// (it's exactly what "submit() was blocked on mb.mu, not on anything
	// else" proves) — the meaningful assertions are that submit() genuinely
	// became the new owner (below) and that it did not do so BEFORE
	// release() returned (proven by the earlier select/time.After).

	submitResult := <-submitDone
	require.True(t, submitResult.becomeOwner, "once drainOrReleaseFinal actually releases mb.mu, the blocked "+
		"submit() must proceed and become the new owner")
	require.Equal(t, uint64(2), submitResult.epoch, "a fresh idle->owned transition must bump the epoch")
}

// TestMailbox_DrainOrReleaseFinal_LegacyReclaimNeverReleasesLock is the
// companion proving the OTHER half of HIGH-1's fix: when the legacy queue
// has something, the OS lock must NOT be released at all — ownership (and
// the lock) is handed straight to the reclaimed turn without ever exposing a
// gap where a different process could steal it (see drainOrReleaseFinal's
// own doc for why release-then-reacquire is rejected).
func TestMailbox_DrainOrReleaseFinal_LegacyReclaimNeverReleasesLock(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "high-1-lock-reclaim-test"

	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() })

	var staleDispatcherCancelCalled bool
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() { staleDispatcherCancelCalled = true },
		current:          generation{id: 1, cancel: func() {}},
	}

	reclaimed := SessionAgentCall{SessionID: sessionID, Prompt: "reclaimed from legacy queue"}
	releaseCalled := false
	checkLegacy := func() (SessionAgentCall, bool) { return reclaimed, true }
	release := func() error {
		releaseCalled = true
		return lk.Release()
	}
	var newDispatcherCancelCalled bool
	newDispatcherCancel := func() { newDispatcherCancelCalled = true }

	next, hasNext, releaseErr := mb.drainOrReleaseFinal(1, checkLegacy, newDispatcherCancel, release)

	require.True(t, hasNext)
	require.Equal(t, reclaimed, next)
	require.NoError(t, releaseErr)
	require.False(t, releaseCalled, "release() must NOT be invoked when the legacy queue reclaims ownership — "+
		"the OS lock stays held for the reclaimed turn, never released-then-reacquired")
	require.Equal(t, mbOwned, mb.state, "state must stay owned across a legacy-queue reclaim")
	require.Equal(t, uint64(1), mb.epoch, "epoch must NOT bump on a same-era reclaim")

	// round 11 review, MEDIUM-1: both cancel handles must be left in a state
	// Cancel()/InterruptAndReplace() can actually use. dispatcherCancel must
	// be the freshly-passed reclaim cancel (NOT nil, and NOT left at
	// whatever it was before), and current.cancel must be nil so Cancel()'s
	// `if genCancel == nil { fallback to dispatcherCancel }` branch actually
	// fires instead of calling the just-finished, already-spent PRIOR turn's
	// stale cancel func (a real second bug this fix's own end-to-end test,
	// TestRun_CancelDuringLegacyReclaimWindow_ActuallyCancelsTurn2, initially
	// caught: dispatcherCancel alone was not sufficient).
	require.NotNil(t, mb.dispatcherCancel, "dispatcherCancel must be populated on reclaim, not left nil")
	// Identity check, not just non-nil (round 12 review, finding B): the
	// fixture's OWN initial dispatcherCancel was also a non-nil func, so a
	// bare NotNil check would pass even if drainOrReleaseFinal silently
	// failed to overwrite it with the freshly-passed reclaim cancel. Calling
	// the field's CURRENT value must invoke the NEW closure, not the stale
	// fixture one.
	mb.dispatcherCancel()
	require.True(t, newDispatcherCancelCalled, "dispatcherCancel must be the freshly-passed reclaim cancel, not left "+
		"at whatever the mailbox held before this call")
	require.False(t, staleDispatcherCancelCalled, "the stale, pre-reclaim dispatcherCancel must not still be reachable")
	require.Nil(t, mb.current.cancel, "current.cancel must be cleared on reclaim — otherwise Cancel() calls the "+
		"stale, already-spent PRIOR turn's cancel func instead of ever reaching the dispatcherCancel fallback")
	require.Equal(t, uint64(1), mb.current.id, "current.id must be preserved (monotonic) even though its cancel is cleared")

	// The OS lock must still be held by THIS process — a second acquisition
	// attempt must fail.
	_, lockErr := session.TryAcquireSessionLock(dataDir, sessionID)
	require.Error(t, lockErr, "the OS lock must remain held across a legacy-queue reclaim")
}

// TestMailbox_DrainOrReleaseFinal_SubmittedBranchClearsStaleCancelHandle is
// the regression test for round 12 review, finding A: the SAME MEDIUM-1
// shape as TestMailbox_DrainOrReleaseFinal_LegacyReclaimNeverReleasesLock
// above, but on drainOrReleaseFinal's OTHER "keep running" branch — the
// mb.submitted queue, populated by the CURRENT (non-legacy) submit() path,
// hit far more often in practice than the legacy-messageQueue fallback.
//
// By the time this branch runs, mb.current.cancel still holds the
// just-finished turn's own genCtx cancel func — already invoked once via
// runTurn's unconditional `cancel()` call immediately before the drain
// (agent.go, right before drainOrReleaseMerged) — inert but NOT nil. Left
// untouched, it defeats Cancel()/InterruptAndReplace()'s
// `current.cancel == nil` fallback gate exactly like the original MEDIUM-1
// defect, for the whole window until the NEXT turn's own beginGeneration
// (which can be as long as title generation takes — several seconds).
//
// Caught by round 12's independent review; confirmed by this session via
// direct code read (mb.submitted branch returned without touching
// mb.current.cancel at all) before landing the one-line fix.
func TestMailbox_DrainOrReleaseFinal_SubmittedBranchClearsStaleCancelHandle(t *testing.T) {
	var staleCancelCalled bool
	queued := SessionAgentCall{SessionID: "s1", Prompt: "queued via submit()"}
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() { staleCancelCalled = true }},
		submitted:        []SessionAgentCall{queued},
	}

	next, hasNext, releaseErr := mb.drainOrReleaseFinal(1, nil, nil, nil)

	require.True(t, hasNext)
	require.Equal(t, queued, next)
	require.NoError(t, releaseErr)
	require.Equal(t, mbOwned, mb.state, "state must stay owned when mb.submitted has a queued call")
	require.Empty(t, mb.submitted, "the popped call must be removed from the queue")

	// THE core finding-A assertion: current.cancel must be cleared so
	// Cancel()'s fallback to dispatcherCancel actually fires, instead of
	// silently invoking the just-finished turn's spent, harmless cancel.
	require.Nil(t, mb.current.cancel, "current.cancel must be cleared on the mb.submitted branch too — otherwise "+
		"Cancel() calls the stale, already-spent PRIOR turn's cancel func instead of ever reaching the "+
		"dispatcherCancel fallback (round 12 review, finding A)")

	// Confirm the ORIGINAL cancel func is genuinely unreachable now, not
	// just that the field looks nil in this snapshot.
	if mb.current.cancel != nil {
		mb.current.cancel()
	}
	require.False(t, staleCancelCalled, "the stale pre-drain cancel func must never be invoked via mb.current.cancel again")
}
