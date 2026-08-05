package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
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

		_, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, lk.Release)
		require.False(t, hasNext)
		require.NoError(t, releaseErr)
		require.Empty(t, orphaned)
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

// TestMailbox_DrainOrReleaseFinal_MuNotHeldDuringRelease_ButNoPrematureIdle
// is the #296/P1-C rewrite of the round-11 structural test
// (TestMailbox_DrainOrReleaseFinal_MuHeldForWholeOfReleaseCallback, which
// asserted the OPPOSITE of the current contract and has been deleted), then
// corrected again per the #297 review: an earlier version of this test
// asserted that work landing during the release() window is handed BACK to
// the original turn loop (state == mbOwned). That is wrong — release() has
// already run by the time the racing submit() is noticed, so the OS lock is
// gone; handing the turn loop another turn to run would mean it runs
// unprotected by any inter-process lock, letting a second process acquire
// the same session lock concurrently. The corrected contract: such work is
// drained into `orphaned` and the era still ends at mbIdle. See
// TestMailbox_DrainOrReleaseFinal_OrphanedWorkNeverRunsWithoutFreshOSLock
// below for the end-to-end proof that a second lock acquisition succeeds
// the instant this call returns.
//
// Before #296, mb.mu was held for the whole of release()'s disk I/O
// (Truncate/Seek/Sync/sidecar-unlink/unlock/Close — no context, no timeout),
// so a slow/hung filesystem, antivirus, or SMB share blocked every other
// in-process reader of the mailbox (submit, Cancel, InterruptAndReplace,
// IsSessionBusy, IsBusy, CancelAll) for that session, unboundedly. #296
// removes that coupling: release() now runs with mb.mu dropped, and
// mb.state == mbReleasing stands in for the mutex as far as HIGH-1's
// atomicity goes.
//
// This test proves the full contract against the exact same paused-release
// setup the old test used:
//  1. mb.mu is NOT held while release() runs — a concurrent submit()
//     completes promptly instead of blocking for the duration of release().
//  2. HIGH-1 still holds — that prompt submit() does NOT become the new
//     owner (mbReleasing reads as busy), and a same-process busy-read
//     observes busy for the whole window.
//  3. The work that landed during the window is neither lost NOR handed
//     back to the (now lock-less) turn loop: it comes back as `orphaned`,
//     and the era ends at mbIdle.
func TestMailbox_DrainOrReleaseFinal_MuNotHeldDuringRelease_ButNoPrematureIdle(t *testing.T) {
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
		orphaned   []SessionAgentCall
	}
	drainDone := make(chan drainResult, 1)
	go func() {
		_, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, release)
		drainDone <- drainResult{hasNext: hasNext, releaseErr: releaseErr, orphaned: orphaned}
	}()

	select {
	case <-releaseEntered:
	case <-drainDone:
		t.Fatal("drainOrReleaseFinal returned before release() was even entered")
	}

	// (1) release() is now paused. mb.mu must NOT be held: a concurrent
	// submit() must complete promptly, not block until release() returns.
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
	case res := <-submitDone:
		// (2) HIGH-1: even though submit() was not blocked, it must not have
		// become the new owner — the OS lock release is still in flight.
		require.False(t, res.becomeOwner, "HIGH-1: while release() is still in flight, a concurrent submit() must "+
			"NOT become the new owner — mbReleasing must read as busy and queue instead")
		require.Equal(t, uint64(0), res.epoch, "a non-owner submit must return epoch 0")
	case <-time.After(2 * time.Second):
		t.Fatal("submit() did not complete while release() was paused — mb.mu is still held during release(), " +
			"reintroducing the unbounded control-plane stall #296 exists to remove")
	}

	// A same-process busy read must also not block, AND must report busy —
	// the actual HIGH-1 property CancelAll/IsBusy/IsSessionBusy depend on.
	busyDone := make(chan bool, 1)
	go func() {
		mb.mu.Lock()
		s := mb.state
		mb.mu.Unlock()
		busyDone <- s != mbIdle
	}()
	select {
	case busy := <-busyDone:
		require.True(t, busy, "the mailbox must read as busy (state != mbIdle) while release() is in flight")
	case <-time.After(2 * time.Second):
		t.Fatal("a mailbox-state read did not complete while release() was paused — mb.mu is still held")
	}

	close(releaseMayReturn)

	// (3) The call submitted during the release window must come back as
	// orphaned — NOT handed back to the turn loop (the OS lock is already
	// gone) and NOT lost.
	select {
	case res := <-drainDone:
		require.False(t, res.hasNext, "work that lands during the release() window must NOT be handed back to "+
			"the turn loop — the OS lock is already released by the time it is noticed")
		require.Equal(t, []SessionAgentCall{{SessionID: "s1", Prompt: "concurrent"}}, res.orphaned,
			"the racing call must be returned as orphaned so the caller can restart it under a fresh OS lock")
		require.NoError(t, res.releaseErr)
	case <-time.After(2 * time.Second):
		t.Fatal("drainOrReleaseFinal did not return after release() completed")
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()
	require.Equal(t, mbIdle, mb.state, "the era must end at mbIdle — there is no OS lock left to keep a turn loop "+
		"running under")
	require.Nil(t, mb.dispatcherCancel)
	require.Nil(t, mb.current.cancel)
}

// TestMailbox_DrainOrReleaseFinal_OrphanedWorkNeverRunsWithoutFreshOSLock is
// the direct regression test for the #297 finding: it proves the actual
// cross-process invariant, not just the in-memory mailbox postcondition.
// While a separate goroutine holds a call that drainOrReleaseFinal reported
// as orphaned, a DIRECT session.TryAcquireSessionLock (standing in for a
// second `crush` process) must succeed immediately — proving no code path
// is still running "as if" it held that lock. Reverting the finalize step to
// the rejected hand-back-to-mbOwned shape must make this test fail: the
// prior draft returned hasNext=true/mbOwned for this exact scenario, and a
// caller trusting that (as drainOrReleaseMerged's turn loop does) would
// start another turn while a concurrent TryAcquireSessionLock now succeeds
// too — two owners of one session.
func TestMailbox_DrainOrReleaseFinal_OrphanedWorkNeverRunsWithoutFreshOSLock(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "p1-c-orphan-no-double-lock-test"

	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)

	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	// release() simulates a concurrent submit() landing during the window —
	// exactly like a real submit() would, it just appends under mb.mu.
	release := func() error {
		mb.mu.Lock()
		mb.submitted = append(mb.submitted, SessionAgentCall{SessionID: sessionID, Prompt: "raced in"})
		mb.mu.Unlock()
		return lk.Release()
	}

	_, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, release)
	require.NoError(t, releaseErr)
	require.False(t, hasNext, "orphaned work must not be reported as hasNext — that would tell the caller's turn "+
		"loop to keep running without a lock")
	require.Len(t, orphaned, 1)

	// THE core #297 assertion: the instant this call returns, the OS lock is
	// genuinely free for a second acquirer — proving nothing is still
	// running "on behalf of" the released lock.
	lk2, lockErr := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, lockErr, "the OS lock must be immediately acquirable by a second holder once "+
		"drainOrReleaseFinal returns — if orphaned work were instead handed back as hasNext=true/mbOwned, a real "+
		"caller would now be running a turn for this session WHILE this second acquisition also succeeds: two "+
		"owners of one session, exactly the bug #297 reported")
	require.NoError(t, lk2.Release())
}

// TestMailbox_DrainOrReleaseFinal_ReleaseErrorStillReachesIdle is the #296/
// P1-C guarantee that a release() returning an error cannot wedge the
// mailbox in mbReleasing forever: it must still finalize to mbIdle (the
// error is surfaced for logging only, never fatal — mirroring how a failed
// Run() defer lk.Release() today only logs).
func TestMailbox_DrainOrReleaseFinal_ReleaseErrorStillReachesIdle(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	release := func() error { return errors.New("disk full") }

	next, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, release)

	require.False(t, hasNext)
	require.Equal(t, SessionAgentCall{}, next)
	require.Empty(t, orphaned)
	require.Error(t, releaseErr, "the release error must be surfaced to the caller for logging")
	require.ErrorContains(t, releaseErr, "disk full")

	mb.mu.Lock()
	defer mb.mu.Unlock()
	require.Equal(t, mbIdle, mb.state, "a failed release must still finalize the mailbox to mbIdle, not wedge it "+
		"in mbReleasing forever")
	require.Nil(t, mb.dispatcherCancel)
	require.Nil(t, mb.current.cancel)
}

// TestMailbox_DrainOrReleaseFinal_ReleasePanicStillReachesIdle is the #296/
// P1-C guarantee that a PANICKING release() also cannot wedge the mailbox:
// the panic is recovered into an error and the mailbox still finalizes to
// mbIdle. Without an internal recover, a panicking release() would propagate
// up through drainOrReleaseFinal on a goroutine with no mb.mu held at the
// panic site to clean up under, leaving the mailbox stuck in mbReleasing —
// permanently "busy" with nobody left to ever finish the transition. The
// test goroutine carries its own recover so an unfixed regression fails at
// the mbIdle assertion below instead of crashing the whole test binary.
func TestMailbox_DrainOrReleaseFinal_ReleasePanicStillReachesIdle(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	release := func() error { panic("release boom") }

	type drainResult struct {
		hasNext    bool
		releaseErr error
		orphaned   []SessionAgentCall
	}
	drainDone := make(chan drainResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				drainDone <- drainResult{
					hasNext:    false,
					releaseErr: fmt.Errorf("panic escaped drainOrReleaseFinal — internal recover missing: %v", r),
				}
			}
		}()
		_, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, release)
		drainDone <- drainResult{hasNext: hasNext, releaseErr: releaseErr, orphaned: orphaned}
	}()

	select {
	case res := <-drainDone:
		require.False(t, res.hasNext)
		require.Empty(t, res.orphaned)
		require.Error(t, res.releaseErr, "the recovered panic must be surfaced as an error")
		require.ErrorContains(t, res.releaseErr, "release boom")
	case <-time.After(2 * time.Second):
		t.Fatal("drainOrReleaseFinal did not return — the panic was not recovered and the mailbox is stuck in mbReleasing")
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()
	require.Equal(t, mbIdle, mb.state, "a panicking release must still finalize the mailbox to mbIdle")
	require.Nil(t, mb.dispatcherCancel)
	require.Nil(t, mb.current.cancel)
}

// TestMailbox_DrainOrReleaseFinal_HardStopDuringReleaseWindow_DiscardsWork
// is the regression test for the #296/P1-C x P0-C interaction: hardStop
// (CancelAll/shutdown) landing WHILE release() is in flight (state ==
// mbReleasing) must not have its shutdown intent silently overridden by
// work that lands in mb.submitted during that same window — neither handed
// back to the turn loop (rejected per #297 — see this function's own doc)
// NOR returned as orphaned to be restarted via a detached Run(), since a
// detached restart is exactly as wrong as a hand-back would be: it starts a
// fresh provider turn while the process is trying to exit, the original
// P0-C bug. Before the finalize step re-checked mb.stopped after
// reacquiring mb.mu, a submit() racing a concurrent hardStop inside the
// release() window would have been handed back to the turn loop as a fresh
// turn.
func TestMailbox_DrainOrReleaseFinal_HardStopDuringReleaseWindow_DiscardsWork(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}

	// Simulate hardStop AND a concurrent submit() both landing during the
	// release() window: the release callback itself (running with mb.mu
	// free) does both, exactly as two separate real goroutines would.
	release := func() error {
		mb.hardStop()
		mb.mu.Lock()
		mb.submitted = append(mb.submitted, SessionAgentCall{SessionID: "s1", Prompt: "raced in during shutdown"})
		mb.mu.Unlock()
		return nil
	}

	next, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, release)

	require.NoError(t, releaseErr)
	require.False(t, hasNext, "work that landed during the release window must NOT be handed back once hardStop "+
		"has latched — a shutdown must not start a fresh turn")
	require.Equal(t, SessionAgentCall{}, next)
	require.Empty(t, orphaned, "work that races in alongside a shutdown must be discarded, not returned as "+
		"orphaned — restarting it via a detached Run() would be exactly the P0-C bug hardStop exists to prevent, "+
		"just reached one step later")

	mb.mu.Lock()
	defer mb.mu.Unlock()
	require.Equal(t, mbIdle, mb.state, "the era must still end at mbIdle even though hardStop landed mid-release")
	require.Nil(t, mb.dispatcherCancel)
	require.Nil(t, mb.current.cancel)
	require.True(t, mb.stopped, "the stopped latch must remain set")
}

// TestSessionAgent_IsBusyAndIsSessionBusy_TreatMbReleasingAsBusy is the
// regression test for the #296/P1-C half of HIGH-1 that lives on
// sessionAgent rather than mailbox: IsBusy() (CancelAll's 5-second shutdown
// drain loop) and IsSessionBusy() (the same-process "can I become owner"
// check) must both report BUSY while a mailbox sits in mbReleasing, not just
// mbOwned. Before this fix, IsBusy() read `mb.state == mbOwned` — mbReleasing
// didn't exist yet when that line was written, but once it does exist,
// mbReleasing fails that equality check and IsBusy() would have reported
// "not busy" for a session whose release() disk I/O (OS session-lock
// teardown) is still genuinely in flight on another goroutine. CancelAll's
// drain loop calling IsBusy() in that window would return early, and
// App.Shutdown proceeds to tear the process's DB down right after CancelAll
// returns — the exact class of race HIGH-1 exists to prevent, reachable
// through the shutdown path instead of the "becomes new owner" path.
// IsSessionBusy() already used `!= mbIdle` before this fix and is included
// here as the non-regression companion proving it stays correct.
//
// Constructs a bare sessionAgent with just the mailboxes map (no provider,
// no DB) since this is testing pure mailbox-state plumbing, not a live turn.
func TestSessionAgent_IsBusyAndIsSessionBusy_TreatMbReleasingAsBusy(t *testing.T) {
	sa := &sessionAgent{mailboxes: csync.NewMap[string, *mailbox]()}
	const sessionID = "releasing-is-busy-test"

	mb := sa.getMailbox(sessionID)
	mb.mu.Lock()
	mb.state = mbReleasing
	mb.mu.Unlock()

	assert.True(t, sa.IsBusy(), "IsBusy must report true while a mailbox is mbReleasing — release() may still be "+
		"holding the OS session lock on another goroutine")
	assert.True(t, sa.IsSessionBusy(sessionID), "IsSessionBusy must report true while mbReleasing, for the same reason")

	mb.mu.Lock()
	mb.state = mbIdle
	mb.mu.Unlock()

	assert.False(t, sa.IsBusy(), "IsBusy must report false once the mailbox is genuinely mbIdle")
	assert.False(t, sa.IsSessionBusy(sessionID), "IsSessionBusy must report false once the mailbox is genuinely mbIdle")
}

// TestMailbox_DrainOrReleaseFinal_SubmittedReclaimNeverReleasesLock is the
// companion proving the OTHER half of HIGH-1's fix: when mb.submitted
// has something, the OS lock must NOT be released at all — ownership (and
// the lock) is handed straight to the reclaimed turn without ever exposing a
// gap where a different process could steal it (see drainOrReleaseFinal's
// own doc for why release-then-reacquire is rejected).
func TestMailbox_DrainOrReleaseFinal_SubmittedReclaimNeverReleasesLock(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "high-1-lock-reclaim-test"

	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() })

	mb := &mailbox{
		state:   mbOwned,
		epoch:   1,
		current: generation{id: 1, cancel: func() {}},
	}

	reclaimed := SessionAgentCall{SessionID: sessionID, Prompt: "reclaimed from submitted"}
	releaseCalled := false
	mb.submitted = []SessionAgentCall{reclaimed}
	release := func() error {
		releaseCalled = true
		return lk.Release()
	}

	next, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, release)

	require.True(t, hasNext)
	require.Equal(t, reclaimed, next)
	require.NoError(t, releaseErr)
	require.Empty(t, orphaned)
	require.False(t, releaseCalled, "release() must NOT be invoked when mb.submitted reclaims ownership — "+
		"the OS lock stays held for the reclaimed turn, never released-then-reacquired")
	require.Equal(t, mbOwned, mb.state, "state must stay owned across a mb.submitted reclaim")
	require.Equal(t, uint64(1), mb.epoch, "epoch must NOT bump on a same-era reclaim")

	// The OS lock must still be held by THIS process — a second acquisition
	// attempt must fail.
	_, lockErr := session.TryAcquireSessionLock(dataDir, sessionID)
	require.Error(t, lockErr, "the OS lock must remain held across a mb.submitted reclaim")
}

// TestMailbox_DrainOrReleaseFinal_SubmittedBranchClearsStaleCancelHandle is
// the regression test for round 12 review, finding A: the SAME MEDIUM-1
// shape as TestMailbox_DrainOrReleaseFinal_SubmittedReclaimNeverReleasesLock
// above, but explicitly checking the stale-cancel-handle postcondition.
// The mb.submitted queue is populated by the CURRENT submit() path and
// is now the only "keep running" branch in drainOrReleaseFinal (the former
// checkLegacy branch was removed in #308).
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
	queued := SessionAgentCall{SessionID: "s1", Prompt: "queued via submit()"}
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
		submitted:        []SessionAgentCall{queued},
	}

	next, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, nil)

	require.True(t, hasNext)
	require.Equal(t, queued, next)
	require.NoError(t, releaseErr)
	require.Empty(t, orphaned)
	require.Equal(t, mbOwned, mb.state, "state must stay owned when mb.submitted has a queued call")
	require.Empty(t, mb.submitted, "the popped call must be removed from the queue")

	// THE core finding-A assertion: current.cancel must be cleared so
	// Cancel()'s fallback to dispatcherCancel actually fires, instead of
	// silently invoking the just-finished turn's spent, harmless cancel.
	require.Nil(t, mb.current.cancel, "current.cancel must be cleared on the mb.submitted branch too — otherwise "+
		"Cancel() calls the stale, already-spent PRIOR turn's cancel func instead of ever reaching the "+
		"dispatcherCancel fallback (round 12 review, finding A)")
}

// TestMailbox_DrainAfterCancel_ClearsStaleCancelHandle is the regression
// test for round 13 review's fourth instance of the same MEDIUM-1 shape:
// drainAfterCancel — the cancel-branch drain, called from agent.go's
// isCancelErr block right before its own `cancel()` call — left
// mb.current.cancel holding the just-cancelled generation's own spent
// (but non-nil) genCtx cancel func on BOTH of its hit branches
// (replacement and submitted), defeating Cancel()/InterruptAndReplace()'s
// current.cancel==nil fallback gate for the window until the replacement
// turn's own beginGeneration — arguably the worst instance of the four,
// since this is exactly the "user already cancelled once and wants the
// replacement instead" path.
func TestMailbox_DrainAfterCancel_ClearsStaleCancelHandle(t *testing.T) {
	t.Run("replacement branch", func(t *testing.T) {
		replacement := SessionAgentCall{SessionID: "s1", Prompt: "replacement"}
		mb := &mailbox{
			state:       mbOwned,
			replacement: &replacement,
			current:     generation{id: 1, cancel: func() {}},
		}

		next, ok := mb.drainAfterCancel()

		require.True(t, ok)
		require.Equal(t, replacement, next)
		require.Nil(t, mb.current.cancel, "current.cancel must be cleared on the replacement branch — otherwise "+
			"Cancel()/InterruptAndReplace() call the stale, already-cancelled PRIOR generation's cancel func "+
			"instead of ever reaching the dispatcherCancel fallback (round 13 review, fourth instance)")
	})

	t.Run("submitted branch", func(t *testing.T) {
		queued := SessionAgentCall{SessionID: "s1", Prompt: "queued"}
		mb := &mailbox{
			state:     mbOwned,
			submitted: []SessionAgentCall{queued},
			current:   generation{id: 1, cancel: func() {}},
		}

		next, ok := mb.drainAfterCancel()

		require.True(t, ok)
		require.Equal(t, queued, next)
		require.Nil(t, mb.current.cancel, "current.cancel must be cleared on the submitted branch too — same "+
			"defect, same fix, mirroring the replacement branch above")
	})
}
