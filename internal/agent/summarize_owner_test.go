package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file contains regression tests for #268 (P0-4): summarization
// working outside session ownership and corrupting live history. Each test
// targets one of the three scenarios the fix must make impossible.
//
// The core invariant tested: beginCompact is an atomic check-and-reserve
// that makes two concurrent compactions (or a compaction concurrent with a
// turn) impossible. The old code used a non-atomic IsSessionBusy check +
// a synthetic "sessionID-summarize" cancel key that could be overwritten.

// TestP268_ManualCompactBlocksConcurrentRun proves scenario 1 is impossible:
// a manual /compact that acquires ownership via beginCompact must block a
// concurrent Run from starting — the Run must queue behind the compaction
// rather than racing it for history writes/deletes.
func TestP268_ManualCompactBlocksConcurrentRun(t *testing.T) {
	sa, sess := newSummarizeCoreTestAgent(t)
	mb := sa.getMailbox(sess.ID)

	// Simulate a manual /compact acquiring ownership.
	epoch, ok := mb.beginCompact(func() {})
	require.True(t, ok, "beginCompact must succeed on idle mailbox")

	// A concurrent Run's submit must see the mailbox as owned and queue.
	becomeOwner, submitEpoch := mb.submit(SessionAgentCall{SessionID: sess.ID, Prompt: "concurrent"}, func() {})
	require.False(t, becomeOwner, "submit must NOT become owner while compaction holds the mailbox")
	require.Zero(t, submitEpoch, "epoch must be 0 when not becoming owner")

	// The queued call must be in submitted, waiting for the compaction to
	// release.
	mb.mu.Lock()
	assert.Equal(t, 1, len(mb.submitted), "the Run's call must be queued in submitted")
	mb.mu.Unlock()

	// Release compaction ownership. The queued call should be handed back.
	hadQueued := mb.abandonOwnership(epoch)
	require.True(t, hadQueued)

	// After release, a new submit can become owner again.
	becomeOwner2, _ := mb.submit(SessionAgentCall{SessionID: sess.ID}, func() {})
	assert.True(t, becomeOwner2, "submit must succeed after compaction released")
}

// TestP268_TwoCompactionsMutuallyExclusive proves scenario 2 is impossible:
// two concurrent compactions of the same session cannot both proceed. The
// second beginCompact must fail because the first already owns the mailbox.
func TestP268_TwoCompactionsMutuallyExclusive(t *testing.T) {
	sa, sess := newSummarizeCoreTestAgent(t)
	mb := sa.getMailbox(sess.ID)

	// First compaction acquires ownership.
	epoch1, ok1 := mb.beginCompact(func() {})
	require.True(t, ok1, "first beginCompact must succeed")
	require.NotZero(t, epoch1)

	// Second compaction must be refused — this is the atomic mutual
	// exclusion that prevents two concurrent compactions from creating
	// overlapping summaries, deleting intersecting rows, and racing for
	// SummaryMessageID.
	_, ok2 := mb.beginCompact(func() {})
	require.False(t, ok2, "second beginCompact must fail — two concurrent compactions are impossible")

	// A manual Summarize call on the same session must see ErrSummarizeQueued.
	err := sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{})
	assert.ErrorIs(t, err, ErrSummarizeQueued, "Summarize must queue when another compaction owns the mailbox")

	// After the first compaction releases, a new one can proceed.
	mb.abandonOwnership(epoch1)
	_, ok3 := mb.beginCompact(func() {})
	require.True(t, ok3, "beginCompact must succeed after the first compaction released")
}

// TestP268_ManualCompactAndTurnMutuallyExclusive proves scenario 3 is
// impossible: a manual /compact and an active turn cannot both own the
// mailbox. If a turn owns (via submit), beginCompact must fail; if a compact
// owns (via beginCompact), submit must queue.
func TestP268_ManualCompactAndTurnMutuallyExclusive(t *testing.T) {
	sa, sess := newSummarizeCoreTestAgent(t)
	mb := sa.getMailbox(sess.ID)

	// Turn acquires ownership first (via submit, exactly like Run's loop).
	becomeOwner, turnEpoch := mb.submit(SessionAgentCall{SessionID: sess.ID}, func() {})
	require.True(t, becomeOwner)

	// Manual /compact's beginCompact must fail — the turn owns.
	_, compactOk := mb.beginCompact(func() {})
	require.False(t, compactOk, "beginCompact must fail while a turn owns the mailbox")

	// The Summarize wrapper must route to the queue.
	err := sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{})
	assert.ErrorIs(t, err, ErrSummarizeQueued)

	// Now release the turn and acquire as compaction instead.
	mb.abandonOwnership(turnEpoch)
	compactEpoch, compactOk2 := mb.beginCompact(func() {})
	require.True(t, compactOk2, "beginCompact must succeed after the turn released")

	// A new turn's submit must queue behind the compaction.
	becomeOwner2, _ := mb.submit(SessionAgentCall{SessionID: sess.ID}, func() {})
	require.False(t, becomeOwner2, "submit must queue while a compaction owns the mailbox")

	mb.abandonOwnership(compactEpoch)
}

// TestP268_CompactionCancelReachableViaMailbox proves the cancel-func for a
// compaction is reachable through mailbox.current.cancel (not a synthetic
// string key). Cancel(sessionID) must cancel the compaction's context.
func TestP268_CompactionCancelReachableViaMailbox(t *testing.T) {
	sa, sess := newSummarizeCoreTestAgent(t)
	mb := sa.getMailbox(sess.ID)

	ctx, cancel := context.WithCancel(t.Context())
	epoch, ok := mb.beginCompact(cancel)
	require.True(t, ok)
	require.NotZero(t, epoch)

	// Cancel must find the compaction's cancel via the SAME path it uses
	// for turns: mb.current.cancel (set by beginCompact).
	mb.mu.Lock()
	genCancel := mb.current.cancel
	mb.mu.Unlock()
	require.NotNil(t, genCancel, "current.cancel must hold the compaction's cancel func")

	// Invoking it must cancel the context.
	genCancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("compaction's cancel func did not cancel its context")
	}

	mb.abandonOwnership(epoch)
	cancel()
}

// TestP268_SummarizeQueueStillWorksAfterFix proves the summarizeQueue path
// (Summarize returning ErrSummarizeQueued when busy) still works correctly
// after the beginCompact migration.
func TestP268_SummarizeQueueStillWorksAfterFix(t *testing.T) {
	sa, sess := newSummarizeCoreTestAgent(t)

	// Simulate a busy session by claiming ownership.
	mb := sa.getMailbox(sess.ID)
	_, ok := mb.beginCompact(func() {})
	require.True(t, ok)

	// Summarize must queue and return ErrSummarizeQueued.
	err := sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{})
	require.ErrorIs(t, err, ErrSummarizeQueued)

	// The queue must hold the request.
	assert.True(t, sa.SummarizeQueued(sess.ID), "summarizeQueue must hold the pending request")
}