package session_test

// P0-2 regression test (found in the final @oh review of tasks #337-349,
// 2026-08-10): run_queue_pump.go's processEntry terminal-failed (deleted)
// any entry that exhausted RunQueueMaxAttempts, including entries that
// only ever failed with SessionLockBusyError — ordinary, expected
// contention from another live process legitimately holding the OS
// session lock for the length of a normal turn. Worse, LeaseRunQueueEntryByID
// AND NackRunQueueEntry both incremented `attempts` per cycle, so
// RunQueueMaxAttempts=10 exhausted after only 5 real retry cycles (~15s at
// the production 3s tick) — a completely ordinary amount of lock
// contention. The result: accepted user work (a queued message, a
// cross-process interrupt, a retried orphaned call) could be silently
// deleted from the durable queue with no error surfaced to the user,
// defeating the entire point of task #340's durable-queue design.
//
// Fixed two ways:
//  1. LeaseRunQueueEntryByID no longer increments attempts (only
//     NackRunQueueEntry does, exactly once per completed failed attempt).
//  2. SessionLockBusyError specifically is nacked via
//     NackRunQueueEntryNoAttemptPenalty, which does not increment attempts
//     at all — lock contention can retry indefinitely without ever
//     exhausting RunQueueMaxAttempts.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// busyThenSuccessCoordinator returns a SessionLockBusyError for its first
// N calls, then succeeds. Simulates a durable-queue entry contending with
// another live process that eventually finishes its own turn and releases
// the lock.
type busyThenSuccessCoordinator struct {
	busyUntilCall int64
	calls         atomic.Int64
}

func (c *busyThenSuccessCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	n := c.calls.Add(1)
	if n <= c.busyUntilCall {
		return nil, &session.SessionLockBusyError{Path: "test-lock-path", HolderPID: 12345}
	}
	var result any = "ok"
	return &result, nil
}

// alwaysFailingCoordinator always returns a plain (non-busy, non-terminal)
// error, simulating a genuinely broken call that should eventually be
// dead-lettered.
type alwaysFailingCoordinator struct {
	calls atomic.Int64
	err   error
}

func (c *alwaysFailingCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, c.err
}

// TestReleaseGate_P0_2_LockBusyNeverExhaustsRetries proves that an entry
// blocked purely by SessionLockBusyError survives far more than
// RunQueueMaxAttempts pump ticks without being terminal-failed (deleted),
// and is executed successfully once the contention clears.
//
// NO EXTERNAL POKE: the test never calls TerminalFailRunQueueEntry,
// NackRunQueueEntry, or processEntry directly — it only starts a real
// RunQueuePump (TestTick for speed) and asserts on the durable queue's
// observable state as the mock coordinator's own internal call counter
// advances.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's executeEntry, remove the `errors.As(err,
//     &busyErr)` branch (or make it fall through to the generic Nack path
//     that increments attempts).
//  2. Run: go test -run TestReleaseGate_P0_2_LockBusyNeverExhaustsRetries -v
//  3. FAIL: the entry is deleted (terminal-failed) after RunQueueMaxAttempts
//     cycles, well before busyUntilCall is reached — require.Eventually
//     for "eventually succeeds" times out because the entry no longer
//     exists to retry.
//  4. Restore the busy-error branch and PASS.
func TestReleaseGate_P0_2_LockBusyNeverExhaustsRetries(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-lock-busy")
	ctx := t.Context()

	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	err = svc.EnqueueRunQueueEntry(ctx, "lock-busy-probe", sess.ID, callDataJSON)
	require.NoError(t, err)

	// Fail with SessionLockBusyError for MANY more calls than
	// RunQueueMaxAttempts (10) — if attempts were still being counted for
	// busy errors, the entry would be deleted long before this.
	coord := &busyThenSuccessCoordinator{busyUntilCall: 25}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "lock-busy-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// The completion signal is the coordinator's OWN call counter exceeding
	// busyUntilCall (25), NOT "pending == 0" — a mid-flight lease (the row
	// is briefly 'leased', not 'pending', while a coordinator.Run() call is
	// in progress) reads identically to "gone" through
	// ListPendingRunQueueEntries, which only scans status='pending'. Using
	// the call counter avoids that ambiguity entirely: it can only reach
	// past busyUntilCall if the coordinator's success branch (n >
	// busyUntilCall) actually ran, which — since busyUntilCall (25) is far
	// past RunQueueMaxAttempts (10) — is only reachable if busy failures
	// never counted as attempts and the entry survived to be retried this
	// many times.
	// This waits on an async count (25 successful pump-tick cycles at a
	// nominal 20ms TestTick, ~500ms in the fast case), not a precise
	// timing relationship -- unlike e.g. the P1-1 lease watchdog margin
	// tests, widening this bound cannot mask the regression it exists to
	// catch: if attempts were still (incorrectly) being counted, calls
	// would plateau at RunQueueMaxAttempts and never reach busyUntilCall
	// regardless of how long we wait. Widened from 5s to 20s after this
	// failed on windows-latest CI (runs 31714546616 and 31718897797,
	// "Condition never satisfied") -- windows-latest is consistently the
	// slowest/most contended runner in this repo's CI matrix, and 25
	// DB-backed lease+nack round trips can legitimately exceed 5s there
	// under -race plus concurrent package load.
	require.Eventually(t, func() bool {
		return coord.calls.Load() > coord.busyUntilCall
	}, 20*time.Second, 20*time.Millisecond,
		"coordinator must eventually be called past busyUntilCall — if this times out, the entry "+
			"was terminal-failed (deleted) before reaching that call count, meaning lock-busy "+
			"failures are still counting toward RunQueueMaxAttempts")

	// Now confirm the entry is actually gone (acked), sustained across
	// several more ticks — not just transiently absent between a lease and
	// its eventual outcome.
	for range 5 {
		time.Sleep(20 * time.Millisecond)
		pending, checkErr := svc.ListPendingRunQueueEntries(ctx)
		require.NoError(t, checkErr)
		require.Empty(t, pending, "entry must be durably acked (gone), not merely transiently leased")
	}
}

// TestReleaseGate_P0_2_GenuineFailureStillExhaustsAfterMaxAttempts proves
// the flip side: a call that genuinely, repeatedly fails with a plain
// (non-busy) retryable error still gets dead-lettered after exactly
// RunQueueMaxAttempts real attempts — not after half that many, which is
// what the pre-fix double-increment (LeaseRunQueueEntryByID AND
// NackRunQueueEntry both incrementing attempts per cycle) produced.
func TestReleaseGate_P0_2_GenuineFailureStillExhaustsAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-genuine-failure")
	ctx := t.Context()

	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	err = svc.EnqueueRunQueueEntry(ctx, "genuine-failure-probe", sess.ID, callDataJSON)
	require.NoError(t, err)

	coord := &alwaysFailingCoordinator{err: assertGenericFailure}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "genuine-failure-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// The primary completion signal is the coordinator's OWN call counter
	// reaching RunQueueMaxAttempts, NOT "pending == 0" —
	// ListPendingRunQueueEntries only scans status='pending', so an entry
	// that's merely transiently 'leased' mid-retry reads identically to
	// "gone". Observed directly: under -race's much heavier
	// per-goroutine-switch overhead, that false positive fired reliably,
	// with a later call-count assertion then seeing only 1 call because
	// the entry hadn't actually been dead-lettered yet — it was just
	// caught between two retry cycles.
	require.Eventually(t, func() bool {
		return coord.calls.Load() >= int64(session.RunQueueMaxAttempts)
	}, 5*time.Second, 20*time.Millisecond,
		"coordinator should be called RunQueueMaxAttempts times before the entry is dead-lettered")

	// Now confirm the entry is actually gone (dead-lettered), sustained
	// across several more ticks — the max-attempts branch terminal-fails
	// without calling the coordinator again, so once calls stops
	// increasing, the row should be deleted and stay deleted.
	//
	// The predicate checks pending-OR-leased (runQueueGoneEverywhere), not
	// pending alone: the preceding wait returns when the 10th Run STARTS —
	// the row is leased at that instant — and a leased row is invisible to
	// ListPendingRunQueueEntries, so a pending-only wait would return on its
	// first poll, before the terminal-fail write. The 100ms sustained loop
	// below happened to cover that gap only because this coordinator's Run
	// is instantaneous; with a slower coordinator both the wait and the
	// loop would pass vacuously while the row was still leased, and the
	// final calls-count assertions would lose their ordering anchor.
	// TerminalFailRunQueueEntry runs only after Run returns, so "gone from
	// both states" is what "dead-lettered" actually means here.
	require.Eventually(t, func() bool {
		gone, checkErr := runQueueGoneEverywhere(ctx, svc)
		return checkErr == nil && gone
	}, 5*time.Second, 20*time.Millisecond, "entry should eventually be dead-lettered (deleted) after exhausting real attempts")
	for range 5 {
		time.Sleep(20 * time.Millisecond)
		pending, checkErr := svc.ListPendingRunQueueEntries(ctx)
		require.NoError(t, checkErr)
		require.Empty(t, pending, "entry must be durably dead-lettered (gone), not merely transiently leased mid-retry")
	}

	calls := coord.calls.Load()
	// With the double-increment bug, this would exhaust at 5 calls (each
	// cycle: Lease +1, Nack +1 = +2 per real attempt, hitting
	// RunQueueMaxAttempts=10 after 5 calls). Fixed, each cycle counts +1, so
	// exhaustion needs 10 real calls before the max-attempts branch
	// terminal-fails it (that check itself doesn't call the coordinator, so
	// the observed call count is exactly RunQueueMaxAttempts, not
	// RunQueueMaxAttempts+1).
	require.GreaterOrEqual(t, calls, int64(9), "entry should survive close to RunQueueMaxAttempts real attempts, not exhaust at half that (the pre-fix double-increment bug)")
	require.LessOrEqual(t, calls, int64(12), "entry should not retry drastically more than RunQueueMaxAttempts real attempts either")
}

var assertGenericFailure = &genericRetryableError{msg: "simulated transient provider failure"}

type genericRetryableError struct{ msg string }

func (e *genericRetryableError) Error() string { return e.msg }
