package agent

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRunQueueTerminalProbe_PendingEmptyIsNotTerminal pins the mechanism behind
// the macOS -race failure of TestReleaseGate_9_DoubleFailureNoDuplicate.
//
// That test waited for "the pump finished with the entry" by polling
// ListPendingRunQueueEntries until it returned nothing. But an entry the pump
// has CLAIMED and is executing right now sits in status='leased', which that
// query does not scan — so the wait returned while the run was still in
// flight. Every message-service call the run then made landed after the
// callCountAtTerminal snapshot and was misread as a retry loop
// (expected 0, actual 3). Fast machines skip past the lease window and never
// see it; the slow -race runner sat inside it.
//
// This test makes the ambiguity deterministic rather than timing-dependent: it
// leases an entry by hand and shows the old predicate reports "gone" while the
// row is very much alive, and that pending-OR-leased does not.
func TestRunQueueTerminalProbe_PendingEmptyIsNotTerminal(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, "run-queue-terminal-probe")
	require.NoError(t, err)

	callData, err := json.Marshal(SessionAgentCall{SessionID: sess.ID, Prompt: "probe"})
	require.NoError(t, err)
	require.NoError(t, env.sessions.EnqueueRunQueueEntry(ctx, "probe-key", sess.ID, callData))

	pending, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the entry starts out pending")
	entryID := pending[0].ID

	// The pump claims it. Nothing about the work is finished — this is the
	// exact instant the old wait condition was returning at.
	leased, err := env.sessions.LeaseRunQueueEntry(ctx, sess.ID, "probe-pump", 30*time.Second)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, entryID, leased.ID)

	// The OLD predicate: satisfied, wrongly.
	pending, err = env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending,
		"a leased entry is invisible to ListPendingRunQueueEntries — this is the false 'terminal' signal")

	// The NEW predicate: the row is still there, because it is.
	require.True(t, entryQueuedAnywhere(t, env, entryID),
		"pending-OR-leased must still see an entry the pump is actively running")

	// And once it is genuinely removed, the new predicate agrees.
	require.NoError(t, env.sessions.TerminalFailRunQueueEntry(ctx, entryID, "probe-pump"))
	require.False(t, entryQueuedAnywhere(t, env, entryID),
		"after terminal-fail the row is gone from both states")
}

// entryQueuedAnywhere mirrors the wait predicate used by
// TestReleaseGate_9_DoubleFailureNoDuplicate: an entry counts as still queued
// while it is pending OR leased. math.MaxInt64 as the staleness cutoff turns
// ListStaleLeasedRunQueueEntries into "every leased entry".
func entryQueuedAnywhere(t *testing.T, env fakeEnv, entryID string) bool {
	t.Helper()
	ctx := t.Context()

	pending, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	for _, e := range pending {
		if e.ID == entryID {
			return true
		}
	}
	leased, err := env.sessions.ListStaleLeasedRunQueueEntries(ctx, math.MaxInt64)
	require.NoError(t, err)
	for _, e := range leased {
		if e.ID == entryID {
			return true
		}
	}
	return false
}
