package agent

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMailbox_AbandonOwnershipAndPopSubmitted_EpochMismatch verifies that
// abandonOwnershipAndPopSubmitted returns nil and touches nothing when the
// epoch has already moved on (a later caller became owner).
func TestMailbox_AbandonOwnershipAndPopSubmitted_EpochMismatch(t *testing.T) {
	t.Parallel()

	mb := &mailbox{
		state: mbOwned,
		epoch: 2,
		current: generation{
			id:     1,
			cancel: func() {},
		},
		dispatcherCancel: func() {},
		submitted: []SessionAgentCall{
			{SessionID: "s1", Prompt: "queued for new era"},
		},
	}

	// Call with stale epoch (1) when current epoch is 2.
	result := mb.abandonOwnershipAndPopSubmitted(1)

	require.Nil(t, result, "must return nil for stale epoch")
	require.Equal(t, mbOwned, mb.state, "state must not change")
	require.Equal(t, uint64(2), mb.epoch, "epoch must not change")
	require.NotNil(t, mb.current.cancel, "current.cancel must not be cleared")
	require.NotNil(t, mb.dispatcherCancel, "dispatcherCancel must not be cleared")
	require.Len(t, mb.submitted, 1, "submitted must not be touched")
	require.Equal(t, "queued for new era", mb.submitted[0].Prompt)
}

// TestMailbox_AbandonOwnershipAndPopSubmitted_NoWork verifies that
// abandonOwnershipAndPopSubmitted correctly transitions to mbIdle and returns
// nil (not an empty slice) when there is no queued work.
func TestMailbox_AbandonOwnershipAndPopSubmitted_NoWork(t *testing.T) {
	t.Parallel()

	mb := &mailbox{
		state: mbOwned,
		epoch: 1,
		current: generation{
			id:     1,
			cancel: func() {},
		},
		dispatcherCancel: func() {},
		submitted:        []SessionAgentCall{},
	}

	result := mb.abandonOwnershipAndPopSubmitted(1)

	require.Nil(t, result, "must return nil for empty queue")
	require.Equal(t, mbIdle, mb.state, "state must be mbIdle")
	require.Equal(t, uint64(1), mb.epoch, "epoch must be preserved")
	require.Nil(t, mb.current.cancel, "current.cancel must be cleared")
	require.Nil(t, mb.dispatcherCancel, "dispatcherCancel must be cleared")
	require.Len(t, mb.submitted, 0, "submitted must be empty")
}

// TestMailbox_AbandonOwnershipAndPopSubmitted_HasWork verifies that
// abandonOwnershipAndPopSubmitted returns all queued entries, clears the queue,
// and transitions to mbIdle.
func TestMailbox_AbandonOwnershipAndPopSubmitted_HasWork(t *testing.T) {
	t.Parallel()

	replacementCall := SessionAgentCall{SessionID: "s1", Prompt: "replacement"}
	mb := &mailbox{
		state: mbOwned,
		epoch: 1,
		current: generation{
			id:     1,
			cancel: func() {},
		},
		dispatcherCancel: func() {},
		submitted: []SessionAgentCall{
			{SessionID: "s1", Prompt: "first"},
			{SessionID: "s1", Prompt: "second"},
			{SessionID: "s1", Prompt: "third"},
		},
		replacement: &replacementCall,
	}

	result := mb.abandonOwnershipAndPopSubmitted(1)

	require.NotNil(t, result, "must return non-nil for non-empty queue")
	require.Len(t, result, 4, "must return all entries including replacement")
	require.Equal(t, "first", result[0].Prompt)
	require.Equal(t, "second", result[1].Prompt)
	require.Equal(t, "third", result[2].Prompt)
	require.Equal(t, "replacement", result[3].Prompt, "replacement must be folded into submitted")

	require.Equal(t, mbIdle, mb.state, "state must be mbIdle")
	require.Equal(t, uint64(1), mb.epoch, "epoch must be preserved")
	require.Nil(t, mb.current.cancel, "current.cancel must be cleared")
	require.Nil(t, mb.dispatcherCancel, "dispatcherCancel must be cleared")
	require.Len(t, mb.submitted, 0, "submitted must be cleared")
	require.Nil(t, mb.replacement, "replacement must be cleared")
}

// TestMailbox_SplitCallSequence_EraBoundaryReorderingReproduces is a
// deterministic repro of the PRE-FIX bug using the still-existing, separate
// abandonOwnership/popAllSubmitted primitives (abandonOwnershipAndPopSubmitted
// does not use this two-call sequence, but the old primitives themselves are
// intentionally left in place for other callers — see their doc comments).
//
// The bug: abandonOwnership and popAllSubmitted are two independent lock
// acquisitions. Anything that runs strictly between the two calls (a new
// submit() becoming the owner of a fresh era, followed by a queue() for that
// era) is invisible to popAllSubmitted's epoch-blind pop, so it gets scooped
// up and handed to the old era's caller instead of being left for the new
// owner's own drain. This test constructs that window explicitly (no
// goroutines needed — the "race" is just "call abandonOwnership, then do
// other things, then call popAllSubmitted", which is exactly the shape
// abandonOwnershipWithHandoff used to have) and confirms the leak happens,
// as documented in popAllSubmitted's doc comment before the P2-5 fix.
func TestMailbox_SplitCallSequence_EraBoundaryReorderingReproduces(t *testing.T) {
	t.Parallel()

	mb := &mailbox{
		state: mbOwned,
		epoch: 1,
		current: generation{
			id:     1,
			cancel: func() {},
		},
		dispatcherCancel: func() {},
		submitted: []SessionAgentCall{
			{SessionID: "s1", Prompt: "old era queued work"},
		},
	}

	hadWork := mb.abandonOwnership(1)
	require.True(t, hadWork, "old era had queued work")
	require.Equal(t, mbIdle, mb.state, "abandonOwnership must leave mbIdle")

	// This is the exact window the bug lived in: abandonOwnership already
	// returned, popAllSubmitted has not run yet. A new owner lands here.
	became, newEpoch := mb.submit(SessionAgentCall{SessionID: "s1", Prompt: "new era first"}, func() {})
	require.True(t, became, "submit on mbIdle must become the new owner")
	require.Equal(t, uint64(2), newEpoch)
	mb.queue(SessionAgentCall{SessionID: "s1", Prompt: "new era second (via queue)"})

	popped := mb.popAllSubmitted()

	var sawNewEraWork bool
	for _, call := range popped {
		if call.Prompt == "new era second (via queue)" {
			sawNewEraWork = true
		}
	}
	require.True(t, sawNewEraWork,
		"documents the pre-fix bug: popAllSubmitted grabs new-era work when "+
			"called as a separate step after abandonOwnership — this is exactly "+
			"why abandonOwnershipWithHandoff must use the atomic "+
			"abandonOwnershipAndPopSubmitted instead of this two-call sequence")
}

// TestMailbox_AbandonOwnershipAndPopSubmitted_EraBoundaryReorderingClosed
// stress-tests the atomic abandonOwnershipAndPopSubmitted method with two
// genuinely concurrent goroutines (no ordering constraint between them, unlike
// a naive test that would serialize "abandon first, then submit" and thereby
// never actually attempt to land inside any window). Because
// abandonOwnershipAndPopSubmitted holds mb.mu for its entire body, mb.submit
// and mb.queue can only ever run strictly before or strictly after it — never
// interleaved — so this test cannot deterministically force the old bug's
// window to be hit (that window no longer exists by construction). What it
// verifies is the invariant that must hold in EITHER legal interleaving:
// whichever call the mutex serializes first determines whether the new-era
// work exists yet — and it is never possible to observe a new era's own
// queue() output smuggled into the old era's pop, across many iterations
// under -race. The real guarantee is architectural (single critical section,
// verified by code review), not statistical; this test is a sanity check
// against a future accidental re-introduction of an early unlock.
func TestMailbox_AbandonOwnershipAndPopSubmitted_EraBoundaryReorderingClosed(t *testing.T) {
	t.Parallel()

	const iterations = 500

	for i := 0; i < iterations; i++ {
		mb := &mailbox{
			state: mbOwned,
			epoch: 1,
			current: generation{
				id:     1,
				cancel: func() {},
			},
			dispatcherCancel: func() {},
			submitted: []SessionAgentCall{
				{SessionID: "s1", Prompt: "old era queued work"},
			},
		}

		var wg sync.WaitGroup
		wg.Add(2)

		var popped []SessionAgentCall
		go func() {
			defer wg.Done()
			popped = mb.abandonOwnershipAndPopSubmitted(1)
		}()
		go func() {
			defer wg.Done()
			// No synchronization with goroutine A: let the scheduler race
			// this against the atomic call for real. submit() itself is a
			// no-op queue-append (not a new owner) if it loses the race and
			// lands while mb.state is still mbOwned — either outcome is
			// legal, both are asserted below.
			mb.submit(SessionAgentCall{SessionID: "s1", Prompt: "new era first"}, func() {})
			mb.queue(SessionAgentCall{SessionID: "s1", Prompt: "new era second (via queue)"})
		}()

		wg.Wait()

		// Whatever the interleaving, the old era's pop must always contain
		// its own pre-existing work.
		require.NotEmpty(t, popped, "old era's own queued work must be popped")
		require.Equal(t, "old era queued work", popped[0].Prompt)

		// The new-era submit()/queue() calls only append to mb.submitted
		// AFTER mb.mu serializes them relative to the atomic call — so if
		// they landed after abandonOwnershipAndPopSubmitted's critical
		// section, their entries remain in mb.submitted for the new owner
		// and must never appear in what the old era already popped. If they
		// landed before it (mb still mbOwned when submit() ran), their
		// entries become part of the OLD era's own queue (submit() appends
		// unconditionally when not mbIdle) and legitimately belong in
		// popped — that is correct handoff behavior, not a leak, since the
		// call was genuinely submitted while the old era still owned the
		// mailbox.
		for _, call := range popped {
			if call.Prompt == "new era second (via queue)" {
				require.Equal(t, uint64(1), mb.epoch,
					"new era second can only legitimately appear in the old "+
						"era's pop if submit() never got a chance to bump the "+
						"epoch before the atomic call ran")
			}
		}
	}
}
