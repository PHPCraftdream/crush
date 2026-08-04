package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// Stage 1 (unwired) unit tests for the mailbox type itself, per
// docs/plans/2026-08-04-session-owner-mailbox-design.md §3-§5. These tests
// exercise mailbox in isolation — no sessionAgent wiring, since nothing
// calls into mailbox yet.

func TestMailbox_Submit_BecomesOwnerWhenIdle(t *testing.T) {
	mb := &mailbox{}
	call := SessionAgentCall{SessionID: "s1", Prompt: "hello"}
	cancelCalled := false
	cancel := func() { cancelCalled = true }

	becomeOwner := mb.submit(call, cancel)

	require.True(t, becomeOwner, "first submit on an idle mailbox must become owner")
	require.Equal(t, mbOwned, mb.state)
	require.Empty(t, mb.submitted, "submitted queue must stay empty when becoming owner directly")
	require.False(t, cancelCalled, "submit must not invoke the dispatcher cancel func itself")
}

func TestMailbox_Submit_QueuesWhenAlreadyOwned(t *testing.T) {
	mb := &mailbox{}
	first := SessionAgentCall{SessionID: "s1", Prompt: "first"}
	second := SessionAgentCall{SessionID: "s1", Prompt: "second"}

	becomeOwner1 := mb.submit(first, func() {})
	require.True(t, becomeOwner1)

	becomeOwner2 := mb.submit(second, func() {})
	require.False(t, becomeOwner2, "submit while owned must queue, not become owner")
	require.Equal(t, mbOwned, mb.state, "state must remain owned")
	require.Len(t, mb.submitted, 1)
	require.Equal(t, second, mb.submitted[0])
}

func TestMailbox_DrainOrRelease_WithQueuedItem(t *testing.T) {
	mb := &mailbox{
		state:     mbOwned,
		current:   generation{id: 3, cancel: func() {}},
		submitted: []SessionAgentCall{{SessionID: "s1", Prompt: "next"}},
	}

	next, ok := mb.drainOrRelease()

	require.True(t, ok, "a queued call must be returned")
	require.Equal(t, "next", next.Prompt)
	require.Empty(t, mb.submitted, "the drained item must be removed from the queue")
	require.Equal(t, mbOwned, mb.state, "state must stay owned when there is more work")
	require.NotZero(t, mb.current.id, "current generation must be left untouched when staying owned")
}

func TestMailbox_DrainOrRelease_EmptyFlipsToIdle(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		dispatcherCancel: func() {},
		current:          generation{id: 7, cancel: func() {}},
	}

	next, ok := mb.drainOrRelease()

	require.False(t, ok, "no queued call must be reported")
	require.Equal(t, SessionAgentCall{}, next)
	require.Equal(t, mbIdle, mb.state, "state must flip to idle only when nothing was queued")
	require.Equal(t, generation{}, mb.current, "current generation must reset to zero value on release")
	require.Nil(t, mb.dispatcherCancel, "dispatcherCancel must be cleared on release")
}

func TestMailbox_InterruptAndReplace_NoOwnerReturnsFalse(t *testing.T) {
	mb := &mailbox{}
	call := SessionAgentCall{SessionID: "s1", Prompt: "replace-me"}

	cancel, hadOwner := mb.interruptAndReplace(call)

	require.False(t, hadOwner, "interruptAndReplace on an idle mailbox must report no owner")
	require.Nil(t, cancel)
	require.Nil(t, mb.replacement, "no replacement should be recorded when there is no owner")
}

func TestMailbox_InterruptAndReplace_OwnedRecordsReplacementAndReturnsCurrentCancel(t *testing.T) {
	currentCancelCalled := false
	currentCancel := func() { currentCancelCalled = true }
	mb := &mailbox{
		state:   mbOwned,
		current: generation{id: 5, cancel: currentCancel},
	}
	call := SessionAgentCall{SessionID: "s1", Prompt: "replace-me"}

	cancel, hadOwner := mb.interruptAndReplace(call)

	require.True(t, hadOwner, "interruptAndReplace on an owned mailbox must report an owner existed")
	require.NotNil(t, mb.replacement, "the replacement must be durably recorded")
	require.Equal(t, call, *mb.replacement)
	require.NotNil(t, cancel, "the current generation's cancel func must be returned")

	// The returned cancel must be the CURRENT generation's cancel, not
	// something else — verify by invoking it and observing the same
	// side effect the original currentCancel would produce.
	cancel()
	require.True(t, currentCancelCalled, "returned cancel must be the current generation's cancel func")
}

func TestMailbox_DrainAfterCancel_PrefersReplacementOverSubmitted(t *testing.T) {
	replacement := SessionAgentCall{SessionID: "s1", Prompt: "replacement"}
	queued := SessionAgentCall{SessionID: "s1", Prompt: "queued"}
	mb := &mailbox{
		replacement: &replacement,
		submitted:   []SessionAgentCall{queued},
	}

	next, ok := mb.drainAfterCancel()

	require.True(t, ok)
	require.Equal(t, replacement, next, "replacement must win over submitted")
	require.Nil(t, mb.replacement, "replacement must be cleared after being drained")
	require.Len(t, mb.submitted, 1, "submitted queue must be untouched when replacement was drained")
}

func TestMailbox_DrainAfterCancel_FallsBackToSubmittedWhenNoReplacement(t *testing.T) {
	queued := SessionAgentCall{SessionID: "s1", Prompt: "queued"}
	mb := &mailbox{
		submitted: []SessionAgentCall{queued},
	}

	next, ok := mb.drainAfterCancel()

	require.True(t, ok)
	require.Equal(t, queued, next)
	require.Empty(t, mb.submitted, "the drained item must be removed from the queue")
}

func TestMailbox_DrainAfterCancel_NeitherReturnsFalse(t *testing.T) {
	mb := &mailbox{}

	next, ok := mb.drainAfterCancel()

	require.False(t, ok)
	require.Equal(t, SessionAgentCall{}, next)
}

func TestMailbox_Inject_StampsCurrentGenerationID(t *testing.T) {
	mb := &mailbox{current: generation{id: 4}}
	msg := message.Message{ID: "m1"}

	mb.inject(msg)

	require.Len(t, mb.injects, 1)
	require.Equal(t, msg, mb.injects[0].msg)
	require.Equal(t, uint64(4), mb.injects[0].afterGenID)
}

func TestMailbox_Inject_ZeroGenerationIsValidStamp(t *testing.T) {
	mb := &mailbox{} // current.id defaults to 0 (no owner yet)
	msg := message.Message{ID: "m0"}

	mb.inject(msg)

	require.Len(t, mb.injects, 1)
	require.Equal(t, uint64(0), mb.injects[0].afterGenID, "0 must be accepted as a meaningful generation stamp")
}

func TestMailbox_DrainInjects_SplitsDueFromFuture(t *testing.T) {
	mb := &mailbox{
		injects: []pendingInject{
			{msg: message.Message{ID: "past"}, afterGenID: 1},
			{msg: message.Message{ID: "current"}, afterGenID: 2},
			{msg: message.Message{ID: "future"}, afterGenID: 3},
		},
	}

	due := mb.drainInjects(2)

	require.Len(t, due, 2, "entries stamped <= genID must be due")
	gotIDs := []string{due[0].msg.ID, due[1].msg.ID}
	require.ElementsMatch(t, []string{"past", "current"}, gotIDs)

	require.Len(t, mb.injects, 1, "future-stamped entries must remain queued")
	require.Equal(t, "future", mb.injects[0].msg.ID)
}

func TestMailbox_DrainInjects_AllDueEmptiesQueue(t *testing.T) {
	mb := &mailbox{
		injects: []pendingInject{
			{msg: message.Message{ID: "a"}, afterGenID: 1},
			{msg: message.Message{ID: "b"}, afterGenID: 1},
		},
	}

	due := mb.drainInjects(5)

	require.Len(t, due, 2)
	require.Empty(t, mb.injects)
}

func TestMailbox_DrainInjects_NoneDueLeavesQueueIntact(t *testing.T) {
	mb := &mailbox{
		injects: []pendingInject{
			{msg: message.Message{ID: "future"}, afterGenID: 10},
		},
	}

	due := mb.drainInjects(2)

	require.Empty(t, due)
	require.Len(t, mb.injects, 1)
}

func TestMailbox_BeginGeneration_IncrementsID(t *testing.T) {
	mb := &mailbox{}

	id1 := mb.beginGeneration(func() {})
	require.Equal(t, uint64(1), id1)

	id2 := mb.beginGeneration(func() {})
	require.Equal(t, uint64(2), id2)

	require.NotEqual(t, id1, id2, "every call must produce a unique id")
}

func TestMailbox_BeginGeneration_RecordsCancelAsCurrent(t *testing.T) {
	mb := &mailbox{}
	called := false
	cancel := func() { called = true }

	genID := mb.beginGeneration(cancel)

	require.Equal(t, genID, mb.current.id)
	mb.current.cancel()
	require.True(t, called, "beginGeneration must record the passed cancel as current.cancel")
}

// A light end-to-end style test tying submit -> interruptAndReplace ->
// drainAfterCancel -> beginGeneration together, mirroring the flow
// described in design §4 (steps 1-4), still entirely within the mailbox's
// own API surface (no sessionAgent involved).
func TestMailbox_InterruptThenDrainAfterCancel_SequenceRoundTrips(t *testing.T) {
	mb := &mailbox{}

	first := SessionAgentCall{SessionID: "s1", Prompt: "first"}
	becomeOwner := mb.submit(first, func() {})
	require.True(t, becomeOwner)

	genCtx, genCancel := context.WithCancel(context.Background())
	genID := mb.beginGeneration(genCancel)
	require.Equal(t, uint64(1), genID)

	replacement := SessionAgentCall{SessionID: "s1", Prompt: "replacement"}
	cancelFn, hadOwner := mb.interruptAndReplace(replacement)
	require.True(t, hadOwner)
	require.NotNil(t, cancelFn)

	cancelFn()
	require.Error(t, genCtx.Err())

	next, ok := mb.drainAfterCancel()
	require.True(t, ok)
	require.Equal(t, replacement, next)
}

// TestMailbox_DrainOrRelease_ConcurrentSubmitInFinalDrainWindow_P0_3 is the
// mandatory deterministic regression test for release-blocker P0-3
// (docs/reviews/2026-08-04-multi-agent-stability-follow-up.md, release-gate
// scenario 3): "a plain concurrent send that lands exactly in the
// final-drain-to-release window must either become the new owner or be
// guaranteed to run under the old owner — never orphaned."
//
// Before this migration, the owner's final drain (messageQueue.PopFront
// returning empty) and the reservation release (activeRequests.Del, run via
// Run()'s deferred releaseSessionReservation) were two SEPARATE, non-atomic
// steps. A concurrent submit landing in the gap between them would observe
// "still busy" (the map entry not yet deleted), append itself to
// messageQueue, and then nobody would ever look at that queue entry again —
// the owner had already decided "nothing queued" and moved on, and the
// concurrent caller had already returned believing its message was queued
// for the (already-departing) owner to pick up.
//
// This test reproduces that EXACT window deterministically — not
// probabilistically — using mailbox.testDrainSeam: a hook drainOrRelease
// invokes strictly after observing mb.submitted empty but strictly before
// flipping mb.state to mbIdle (i.e., precisely inside what is now one
// critical section but used to be the gap between two). Because
// mb.mu is held for the whole of drainOrRelease (including the hook call),
// a concurrent submit() blocks on mb.mu.Lock() until the hook returns and
// drainOrRelease's own critical section finishes — so the test does not
// need to win a race against goroutine scheduling to land the submit inside
// the window; the window is held open by the mutex until the test releases
// it.
func TestMailbox_DrainOrRelease_ConcurrentSubmitInFinalDrainWindow_P0_3(t *testing.T) {
	mb := &mailbox{
		state:   mbOwned,
		current: generation{id: 1, cancel: func() {}},
	}

	seamEntered := make(chan struct{})
	releaseSeam := make(chan struct{})
	mb.testDrainSeam = func() {
		close(seamEntered)
		<-releaseSeam
	}

	var (
		wg                    sync.WaitGroup
		concurrentBecameOwner bool
	)
	concurrentCall := SessionAgentCall{SessionID: "s1", Prompt: "concurrent-during-final-drain"}

	// Owner's final drain, running in its own goroutine so the test can
	// observe it paused mid-critical-section via seamEntered.
	drainResult := make(chan struct {
		next SessionAgentCall
		ok   bool
	}, 1)
	wg.Go(func() {
		next, ok := mb.drainOrRelease()
		drainResult <- struct {
			next SessionAgentCall
			ok   bool
		}{next, ok}
	})

	// Wait until drainOrRelease has entered the hook — i.e. has already
	// observed mb.submitted empty and is about to flip state to mbIdle, but
	// has NOT released mb.mu yet (the hook itself runs under mb.mu, and
	// mb.mu is not released until drainOrRelease's deferred Unlock runs
	// after the hook returns).
	select {
	case <-seamEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("drainOrRelease never reached the test seam")
	}

	// Fire the concurrent submit NOW, while drainOrRelease is paused inside
	// its own critical section holding mb.mu. submit() must block on
	// mb.mu.Lock() until we release the seam below — proving the two
	// critical sections cannot interleave.
	submitReturned := make(chan struct{})
	wg.Go(func() {
		concurrentBecameOwner = mb.submit(concurrentCall, func() {})
		close(submitReturned)
	})

	// submit() must NOT be able to proceed while the seam is held — give it
	// a moment on a real scheduler and confirm it is still blocked. This
	// turns "the two critical sections are mutually exclusive" from an
	// assumption into an observed fact for this run.
	select {
	case <-submitReturned:
		t.Fatal("submit() returned before drainOrRelease released the mailbox lock — critical sections overlapped")
	case <-time.After(50 * time.Millisecond):
	}

	// Now let drainOrRelease finish its critical section (flip to idle,
	// unlock). Exactly one of two outcomes is correct from here:
	//  1. drainOrRelease's own critical section commits FIRST (state ->
	//     mbIdle, lock released) and submit() then acquires the lock and
	//     sees mbIdle -> becomes the new owner (concurrentBecameOwner ==
	//     true). This is what will actually happen here, since submit() is
	//     already blocked waiting on mb.mu when we release the seam.
	// Either way, the message is never orphaned: it either becomes owned by
	// the concurrent caller (fresh Run()) or would have been picked up by
	// the departing owner had it arrived a moment earlier (covered by the
	// sibling "WithQueuedItem" test above). What must NEVER happen is what
	// the old code allowed: the departing owner decides "nothing queued"
	// AND the concurrent caller decides "still busy, I'll just queue" —
	// both true at once, with nobody left to drain the queue.
	close(releaseSeam)
	wg.Wait()

	res := <-drainResult
	require.False(t, res.ok, "the owner's own drain found nothing queued at the instant it checked")
	require.True(t, concurrentBecameOwner,
		"the concurrent submit landing in the old lost-wakeup window must become the new owner instead of being silently queued with nobody left to drain it")
	require.Equal(t, mbOwned, mb.state, "the concurrent submit becoming owner must leave the mailbox owned, not idle")
	require.Empty(t, mb.submitted, "the concurrent call must have been picked up as the new owner's call, not left sitting in submitted with no reader")
}

// TestMailbox_DrainOrRelease_NoSeamHook_BehavesIdentically is a sanity check
// that a nil testDrainSeam (the case for every real, non-test mailbox) makes
// drainOrRelease behave exactly as it did before the hook was added — the
// hook must be a strict no-op addition, never a behavior change.
func TestMailbox_DrainOrRelease_NoSeamHook_BehavesIdentically(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		dispatcherCancel: func() {},
		current:          generation{id: 9, cancel: func() {}},
	}

	next, ok := mb.drainOrRelease()

	require.False(t, ok)
	require.Equal(t, SessionAgentCall{}, next)
	require.Equal(t, mbIdle, mb.state)
}
