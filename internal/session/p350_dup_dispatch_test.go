package session_test

// P350 regression tests (found in the third @oh review pass over #337-349,
// 2026-08-10): run_queue_pump.go's executeEntry treated Coordinator.Run's
// nil error as unconditional success and Acked (deleted) the durable row
// regardless of whether the call actually ran. agent.sessionAgent.Run
// returns (nil, nil) — not an error — when the target session is already
// owned by a live, in-process turn: the call is merely appended to that
// owner's mailbox queue (mailbox.submit unconditionally appends, no dedup),
// not executed by this call.
//
// This was reachable two ways, both closed by this round's fix:
//  1. Self-inflicted: RunQueueLeaseTTL (30s) is far shorter than a real LLM
//     turn can take, and executeEntry never renewed its lease while
//     Coordinator.Run was in flight, so CleanupExpiredLeases could return
//     an entry to pending while the goroutine executing it was still
//     genuinely running — the next tick then leased and dispatched a
//     SECOND goroutine for the same session.
//  2. Same-tick: tick() leases and `go executeEntry`-dispatches entries one
//     at a time in a single pass; two distinct durably-queued entries for
//     the same session (e.g. two calls queued while a process was down)
//     could be leased and dispatched back to back within the same tick,
//     before either had run long enough to matter.
//
// Fixed two ways:
//  1. RunQueuePump.inFlight tracks session IDs with an executeEntry
//     goroutine currently running FROM THIS PUMP INSTANCE; processEntry
//     refuses to lease a pending entry for a session already in that set.
//     This closes both paths above at the source — the pump itself can
//     never concurrently dispatch two entries for one session.
//  2. session.ErrCallQueuedNotExecuted is returned by
//     coordinatorAdapterImpl.Run when the underlying call returns (nil,
//     nil) — i.e. queued into a genuinely EXTERNAL live owner the inFlight
//     guard has no visibility into (e.g. a concurrent web/CLI request in
//     the same process). executeEntry treats this specially: it must NOT
//     Ack (the row would be deleted for work that has not actually run)
//     and must NOT immediately Nack-and-retry either (mailbox.submit
//     appends unconditionally on every call, so retrying every tick would
//     append a new duplicate on every attempt). The entry is left exactly
//     as leased; RunQueueLeaseTTL's natural expiry is the only recovery
//     path.

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// concurrencyTrackingCoordinator blocks every call on a shared gate (open
// by default) and tracks, per session ID, whether more than one call is
// ever in flight at the same time — the exact condition the inFlight guard
// must prevent.
type concurrencyTrackingCoordinator struct {
	mu             sync.Mutex
	activePerSess  map[string]int
	sawConcurrency bool

	gateMu sync.Mutex
	closed bool
	gate   chan struct{}

	calls atomic.Int64
}

func newConcurrencyTrackingCoordinator() *concurrencyTrackingCoordinator {
	return &concurrencyTrackingCoordinator{
		activePerSess: make(map[string]int),
		gate:          make(chan struct{}),
	}
}

// hold blocks all in-flight (and future) calls until release is called.
func (c *concurrencyTrackingCoordinator) hold() {
	c.gateMu.Lock()
	defer c.gateMu.Unlock()
	c.closed = true
}

func (c *concurrencyTrackingCoordinator) release() {
	c.gateMu.Lock()
	defer c.gateMu.Unlock()
	if c.closed {
		close(c.gate)
		c.closed = false
	}
}

func (c *concurrencyTrackingCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)

	c.mu.Lock()
	c.activePerSess[callData.SessionID]++
	if c.activePerSess[callData.SessionID] > 1 {
		c.sawConcurrency = true
	}
	c.mu.Unlock()

	// Block here if the gate is currently held, so the test can force an
	// overlap window that would expose a missing inFlight guard.
	c.gateMu.Lock()
	gate := c.gate
	closed := c.closed
	c.gateMu.Unlock()
	if closed {
		<-gate
	}

	c.mu.Lock()
	c.activePerSess[callData.SessionID]--
	c.mu.Unlock()

	var result any = "ok"
	return &result, nil
}

// TestReleaseGate_P350_NoDuplicateDispatchForSameSession proves that two
// distinct durably-queued entries for the SAME session are never dispatched
// concurrently by one pump instance — the same-tick path to the bug
// described in this file's top comment.
//
// NO EXTERNAL POKE: only a real RunQueuePump (TestTick for speed) drives
// execution; the test observes outcomes through the coordinator's own
// concurrency tracking and the durable queue's state.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's processEntry, remove the inFlight busy-check
//     block (the one returning early when the session is already busy).
//  2. Run: go test -run TestReleaseGate_P350_NoDuplicateDispatchForSameSession -v -race -count=5
//  3. FAIL (or race-detected, depending on timing): sawConcurrency becomes
//     true — both entries' coordinator.Run calls overlap.
//  4. Restore the inFlight guard and PASS.
func TestReleaseGate_P350_NoDuplicateDispatchForSameSession(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-dup-dispatch")
	ctx := t.Context()

	mkCallData := func(prompt string) []byte {
		callData := map[string]any{"SessionID": sess.ID, "Prompt": prompt}
		callDataJSON, err := json.Marshal(callData)
		require.NoError(t, err)
		return callDataJSON
	}

	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "dup-dispatch-probe-1", sess.ID, mkCallData("first queued call")))
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "dup-dispatch-probe-2", sess.ID, mkCallData("second queued call")))

	coord := newConcurrencyTrackingCoordinator()
	coord.hold() // force any overlapping dispatch to actually overlap, not race past by luck

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "dup-dispatch-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Let the first call start and several more ticks elapse while it is
	// held — if the inFlight guard is missing, the second entry gets
	// leased and dispatched during this window, overlapping the first.
	require.Eventually(t, func() bool {
		return coord.calls.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "first call must start")
	time.Sleep(300 * time.Millisecond) // ~15 ticks at 20ms

	coord.mu.Lock()
	callsWhileHeld := coord.calls.Load()
	sawConcurrencyMidHold := coord.sawConcurrency
	coord.mu.Unlock()
	require.Equal(t, int64(1), callsWhileHeld, "only ONE of the two entries should have been dispatched while the first is still in flight — the second must wait for inFlight to clear")
	require.False(t, sawConcurrencyMidHold, "no two calls for the same session should ever run concurrently")

	coord.release()

	// Now both entries should complete in sequence.
	require.Eventually(t, func() bool {
		return coord.calls.Load() >= 2
	}, 2*time.Second, 10*time.Millisecond, "second call must eventually start once the first releases")

	require.Eventually(t, func() bool {
		pending, err := svc.ListPendingRunQueueEntries(ctx)
		if err != nil {
			return false
		}
		return len(pending) == 0
	}, 2*time.Second, 10*time.Millisecond, "both entries must eventually be acked")

	coord.mu.Lock()
	defer coord.mu.Unlock()
	require.False(t, coord.sawConcurrency, "no two calls for the same session must ever have run concurrently, start to finish")
	require.Equal(t, int64(2), coord.calls.Load(), "exactly two calls total — one per queued entry, no duplicates")
}

// queuedNotExecutedCoordinator always reports the call as queued into an
// external owner, never actually executing it.
type queuedNotExecutedCoordinator struct {
	calls atomic.Int64
}

func (c *queuedNotExecutedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, session.ErrCallQueuedNotExecuted
}

// TestReleaseGate_P350_QueuedNotExecutedNeitherAcksNorSpamRetries proves
// that when Coordinator.Run reports ErrCallQueuedNotExecuted, executeEntry
// does not Ack (delete) the durable row — since the work has not actually
// run — and does not immediately retry it either (which would append a new
// duplicate to the external owner's mailbox on every tick).
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's executeEntry, remove the
//     `errors.Is(err, ErrCallQueuedNotExecuted)` branch (falls through to
//     the generic Nack path).
//  2. Run: go test -run TestReleaseGate_P350_QueuedNotExecutedNeitherAcksNorSpamRetries -v
//  3. FAIL: calls grows well past 1 across several ticks (spam-retried)
//     instead of staying pinned at 1.
//  4. Restore the branch and PASS.
func TestReleaseGate_P350_QueuedNotExecutedNeitherAcksNorSpamRetries(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-queued-not-executed")
	ctx := t.Context()

	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "queued-not-executed-probe", sess.ID, callDataJSON))

	coord := &queuedNotExecutedCoordinator{}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "queued-not-executed-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	require.Eventually(t, func() bool {
		return coord.calls.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "the entry must be leased and attempted at least once")

	// Let many more ticks elapse. RunQueueLeaseTTL is 30s (not test-
	// overridable), so within this window the lease cannot have naturally
	// expired — if the entry were being Nacked back to pending instead of
	// left leased, it would be re-leased and re-dispatched on nearly every
	// tick, and calls would grow well past 1.
	time.Sleep(300 * time.Millisecond) // ~15 ticks at 20ms
	require.Equal(t, int64(1), coord.calls.Load(), "must not be retried while still within its lease window — retrying would append a duplicate to the external owner's mailbox on every attempt")

	// Must not have been Acked (deleted) either: it should still exist,
	// durably, in the 'leased' state. ListStaleLeasedRunQueueEntries with a
	// far-future cutoff matches ANY currently-leased entry regardless of
	// whether its lease has actually expired yet.
	farFuture := time.Now().Add(time.Hour).Unix()
	leased, err := svc.ListStaleLeasedRunQueueEntries(ctx, farFuture)
	require.NoError(t, err)
	require.Len(t, leased, 1, "the entry must still exist in the leased state — not acked/deleted for work that never actually ran")
	require.Equal(t, sess.ID, leased[0].SessionID)

	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "must not have been nacked back to pending either — that would let the very next tick re-dispatch and append another duplicate")
}
