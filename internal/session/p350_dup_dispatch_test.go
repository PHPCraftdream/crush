package session_test

// P350 regression tests (found across the third and fourth @oh review
// passes over #337-349, 2026-08-10): run_queue_pump.go's executeEntry
// treated Coordinator.Run's nil error as unconditional success and Acked
// (deleted) the durable row regardless of whether the call actually ran.
// agent.sessionAgent.Run returns (nil, nil) — not an error — when the
// target session is already owned by a live, in-process turn: the call is
// merely appended to that owner's mailbox queue (mailbox.submit
// unconditionally appends, no dedup), not executed by this call.
//
// This was reachable three ways:
//  1. Same-tick (third pass): tick() leases and `go executeEntry`-dispatches
//     entries one at a time in a single pass; two distinct durably-queued
//     entries for the same session (e.g. two calls queued while a process
//     was down) could be leased and dispatched back to back within the
//     same tick, before either had run long enough to matter.
//  2. Sequential self-inflicted (found by the third pass, actually fixed by
//     the fourth): RunQueueLeaseTTL (30s) is far shorter than a real LLM
//     turn can take. The third pass's inFlight guard (below) does NOT
//     close this path on its own — inFlight only blocks a SECOND
//     concurrent dispatch while the first goroutine is still tracked as
//     busy; it does nothing about the underlying DB row silently flipping
//     from 'leased' to 'pending' out from under a still-running execution.
//     Without lease renewal, CleanupExpiredLeases would do exactly that
//     after 30s, and the ORIGINAL goroutine's own eventual AckRunQueueEntry
//     (`WHERE status = leased`) would then silently fail to match (logged,
//     not delete anything), leaving the row pending for a LATER tick to
//     lease and dispatch — a genuine duplicate execution of the same turn,
//     sequential rather than concurrent, but exactly as harmful.
//  3. External busy-then-recovered (found by the fourth pass): the original
//     fix for ErrCallQueuedNotExecuted (see below) left the entry exactly
//     as leased and untouched, relying solely on CleanupExpiredLeases's
//     natural lease-expiry as its recovery path — but that same cleanup
//     unconditionally increments attempts on every recovery. A session
//     that stayed externally busy for RunQueueMaxAttempts lease windows (a
//     few minutes) would have its accepted, never-actually-failed work
//     silently dead-lettered (deleted) — the same class of bug
//     SessionLockBusyError's no-attempt-penalty handling exists to prevent
//     for the equivalent cross-process OS-lock case.
//
// Fixed:
//  1. RunQueuePump.inFlight tracks session IDs with an executeEntry
//     goroutine currently running FROM THIS PUMP INSTANCE; processEntry
//     refuses to lease a pending entry for a session already in that set.
//     Closes path 1 above at the source — the pump itself can never
//     concurrently dispatch two entries for one session.
//  2. executeEntry now runs a renewal loop (ticker at leaseTTL/3) alongside
//     Coordinator.Run, calling the new RenewRunQueueLease query to push
//     lease_expires_at forward — scoped to `status = leased AND leased_by
//     = ?` so a lease already reassigned to a different owner is never
//     silently extended out from under it. Closes path 2: a still-running
//     execution's lease can no longer expire underneath it under normal
//     scheduling.
//  3. session.ErrCallQueuedNotExecuted is returned by
//     coordinatorAdapterImpl.Run when the underlying call returns (nil,
//     nil) — i.e. queued into a genuinely EXTERNAL live owner the inFlight
//     guard has no visibility into. executeEntry treats this specially: no
//     Ack (the row would be deleted for work that has not actually run),
//     and — closing path 3 — an immediate
//     NackRunQueueEntryNoAttemptPenalty release (never counts an attempt,
//     mirroring SessionLockBusyError's own handling) paired with a LOCAL
//     RunQueuePump.busyBackoffUntil deadline so THIS pump instance does not
//     immediately re-lease and re-dispatch the same entry on the very next
//     tick — mailbox.submit appends unconditionally on every call, so an
//     uncontrolled retry loop would append a new duplicate on every
//     attempt. A single RenewRunQueueLease call was tried first and did NOT
//     work: it happens almost instantly after the original lease, barely
//     extending lease_expires_at beyond what leasing already set, so
//     CleanupExpiredLeases still reaped the row (and charged an attempt)
//     after essentially one ordinary TTL window — same as doing nothing.
//
// RunQueueLeaseTTL itself (30s) is not test-overridable, which made path 2
// impossible to exercise deterministically and quickly with a real timed
// test — RunQueuePumpConfig.TestLeaseTTL (mirroring the existing TestTick
// seam) was added to close that gap; see
// TestReleaseGate_P350_LeaseRenewedDuringLongExecution and
// TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty below.

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
// duplicate to the external owner's mailbox on every tick): the row is
// released (visible as pending again, matching SessionLockBusyError's own
// no-penalty handling) but this pump instance's local busyBackoffUntil
// deadline prevents IT from re-leasing the same session again immediately.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's executeEntry, remove the
//     `errors.Is(err, ErrCallQueuedNotExecuted)` branch (falls through to
//     the generic Nack path, which DOES increment attempts — a different,
//     already-covered regression) — or, to specifically target the
//     no-immediate-retry guarantee this test checks, remove just the
//     `p.busyBackoffUntil[...] = ...` line while keeping the
//     NackRunQueueEntryNoAttemptPenalty call.
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
		TestLeaseTTL:   500 * time.Millisecond,
	})
	pump.Start()
	defer pump.Stop()

	require.Eventually(t, func() bool {
		return coord.calls.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "the entry must be leased and attempted at least once")

	// Let many more ticks elapse, well within the local busy-backoff window
	// (500ms) — if the entry were being re-leased and re-dispatched on
	// every tick instead of respecting the local backoff, calls would grow
	// well past 1.
	time.Sleep(300 * time.Millisecond) // ~15 ticks at 20ms, still < 500ms backoff
	require.Equal(t, int64(1), coord.calls.Load(), "must not be retried while still within its local busy-backoff window — retrying would append a duplicate to the external owner's mailbox on every attempt")

	// Must not have been Acked (deleted) either: it should still exist,
	// durably, released back to pending (not leased forever, not gone).
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the entry must still exist, released back to pending — not acked/deleted for work that never actually ran")
	require.Equal(t, sess.ID, pending[0].SessionID)
	// This assertion has the same fast-round-trip timing dependency as
	// TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty's
	// own attempts==0 check — see that test's doc comment for why.
	require.Equal(t, int64(0), pending[0].Attempts, "must not have incurred an attempt penalty for external contention")
}

// slowCoordinator blocks every call until release is closed, tracking how
// many times it has been entered. Simulates a real, long-running LLM turn.
type slowCoordinator struct {
	calls   atomic.Int64
	release chan struct{}
}

func (c *slowCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	<-c.release
	var result any = "ok"
	return &result, nil
}

// TestReleaseGate_P350_LeaseRenewedDuringLongExecution proves that a call
// held in flight across SEVERAL lease TTL windows is executed exactly once
// — the lease-renewal loop must keep the entry's row genuinely 'leased' for
// the whole duration, so CleanupExpiredLeases never returns it to pending
// while it is still running, and no later tick dispatches a duplicate.
//
// Uses RunQueuePumpConfig.TestLeaseTTL to make RunQueueLeaseTTL's normally
// non-overridable 30s window fast enough to actually cross multiple times
// within a test's real-time budget. TestLeaseTTL is kept at a full second
// (not sub-second): lease_expires_at is stored as Unix SECONDS (see
// internal/db/migrations/20260809000001_add_session_run_queue.sql and every
// existing `.Unix()`-based computation in run_queue_pump.go/session.go), so
// a sub-second TTL is silently truncated to 0 by `int64(ttl.Seconds())` in
// LeaseRunQueueEntry and produces non-deterministic, boundary-dependent
// behavior — confirmed by hand: an earlier version of this test using
// TestLeaseTTL=150ms failed unpredictably for exactly this reason. A whole
// 1-second TTL sidesteps the truncation (adding exactly 1 second to `now`
// always advances `.Unix()` by exactly 1, regardless of the sub-second
// offset within the current second) at the cost of a slower test.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's executeEntry, disable the renewal loop, e.g.
//     change `case <-ticker.C:` to `case <-(chan time.Time)(nil):` (never
//     fires) or wrap the ticker-case body in `if false {`.
//  2. Run: go test -run TestReleaseGate_P350_LeaseRenewedDuringLongExecution -v -race -count=5
//  3. FAIL: coord.calls ends up >= 2 — the entry was re-leased and
//     re-dispatched mid-execution once its lease expired unrenewed.
//  4. Restore the renewal loop and PASS.
func TestReleaseGate_P350_LeaseRenewedDuringLongExecution(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-lease-renewal")
	ctx := t.Context()

	callData := map[string]any{"SessionID": sess.ID, "Prompt": "long running call"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "lease-renewal-probe", sess.ID, callDataJSON))

	coord := &slowCoordinator{release: make(chan struct{})}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "lease-renewal-pump",
		TestTick:       func() time.Duration { return 50 * time.Millisecond },
		TestLeaseTTL:   1 * time.Second,
	})
	pump.Start()
	defer pump.Stop()

	require.Eventually(t, func() bool {
		return coord.calls.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "the entry must be leased and execution started")

	// Hold the call in flight across several TTL windows (1s each). Note:
	// this specific mid-hold assertion alone does NOT discriminate renewal
	// from no-renewal — the inFlight guard (third pass) already blocks a
	// same-tick/self-race duplicate for as long as this goroutine is
	// tracked as running, regardless of whether the DB lease itself has
	// expired underneath it. The assertion below THIS one (after
	// coord.release is closed) is what actually distinguishes the two: if
	// renewal never happened, the row already flipped to pending during
	// this sleep, and closing the gate lets the ALREADY-DISPATCHED first
	// goroutine finish while a SECOND, independently-leased goroutine gets
	// to run too — see the revert-check procedure above, which fails at
	// that later assertion, not this one.
	time.Sleep(3500 * time.Millisecond) // ~3.5 TTL windows at 1s
	require.Equal(t, int64(1), coord.calls.Load(), "no second dispatch should have occurred while the first call is still genuinely in flight, across multiple TTL windows")

	close(coord.release)

	require.Eventually(t, func() bool {
		pending, checkErr := svc.ListPendingRunQueueEntries(ctx)
		if checkErr != nil {
			return false
		}
		return len(pending) == 0
	}, 2*time.Second, 10*time.Millisecond, "entry should be acked once the long call finally completes")

	// Sustained check, matching this file's established pattern for
	// distinguishing "durably gone" from "transiently leased".
	for range 5 {
		time.Sleep(20 * time.Millisecond)
		pending, checkErr := svc.ListPendingRunQueueEntries(ctx)
		require.NoError(t, checkErr)
		require.Empty(t, pending)
	}

	require.Equal(t, int64(1), coord.calls.Load(), "the long-running call must have been executed exactly once — renewal must have kept its lease alive across multiple TTL windows, preventing a duplicate dispatch")
}

// queuedNotExecutedThenSuccessCoordinator returns ErrCallQueuedNotExecuted
// for its first N calls, then succeeds — simulating a session that stays
// externally busy (owned by another live, in-process turn) for a while
// before freeing up.
type queuedNotExecutedThenSuccessCoordinator struct {
	busyUntilCall int64
	calls         atomic.Int64
}

func (c *queuedNotExecutedThenSuccessCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	n := c.calls.Add(1)
	if n <= c.busyUntilCall {
		return nil, session.ErrCallQueuedNotExecuted
	}
	var result any = "ok"
	return &result, nil
}

// TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty proves
// that an entry blocked purely by ErrCallQueuedNotExecuted (a genuinely
// external live owner) survives far more than RunQueueMaxAttempts local
// backoff cycles without being dead-lettered (deleted), and is executed
// successfully once the external owner frees up.
//
// Unlike TestReleaseGate_P350_LeaseRenewedDuringLongExecution, this backoff
// is tracked purely in-memory (RunQueuePump.busyBackoffUntil, a time.Time,
// not a Unix-seconds DB column), so a fast TestLeaseTTL is used here.
//
// Honest caveat (found by the fifth @oh review pass): the fast TestLeaseTTL
// (30ms) DOES still hit the same second-granularity truncation as the
// initial LeaseRunQueueEntry call (`int64((30ms).Seconds())` == 0), so the
// row's own lease_expires_at is effectively "now" the instant it is leased —
// the assertion below that attempts stays exactly 0 relies on this test's
// own lease→Nack round trip completing faster than one CleanupExpiredLeases
// cycle (one pump tick, 20ms here), not on a hard timing invariant. This has
// not been observed to flake across many runs (the round trip is a fast,
// local, synchronous call chain), but a slow/loaded CI runner could in
// principle interleave a cleanup pass between lease and Nack and charge one
// spurious attempt. If this test ever flakes on `attempts == 0`, that is the
// mechanism to suspect first — not a regression in the backoff fix itself.
//
// A first version of this fix tried achieving backoff via a single
// RenewRunQueueLease call instead; that failed this very test (attempts
// still reached RunQueueMaxAttempts in the ordinary ~10 TTL windows) because
// the renewal happens almost instantly after the original lease was taken,
// barely extending lease_expires_at beyond what leasing already set.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's executeEntry, replace the
//     NackRunQueueEntryNoAttemptPenalty + busyBackoffUntil branch under
//     `errors.Is(err, ErrCallQueuedNotExecuted)` with a no-op (or restore
//     the single RenewRunQueueLease call — either reproduces the bug).
//  2. Run: go test -run TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty -v -race
//  3. FAIL: the entry is dead-lettered (deleted) well before busyUntilCall
//     is reached — require.Eventually for "eventually called past
//     busyUntilCall" times out.
//  4. Restore the fix and PASS.
func TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-queued-backoff")
	ctx := t.Context()

	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "queued-backoff-probe", sess.ID, callDataJSON))

	// Far more than RunQueueMaxAttempts (10) — if ErrCallQueuedNotExecuted
	// still counted as an attempt (the pre-fix behavior), the entry would
	// be dead-lettered long before reaching this many cycles.
	coord := &queuedNotExecutedThenSuccessCoordinator{busyUntilCall: 25}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "queued-backoff-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
		TestLeaseTTL:   30 * time.Millisecond,
	})
	pump.Start()
	defer pump.Stop()

	// See TestReleaseGate_P0_2_LockBusyNeverExhaustsRetries (same shape,
	// same 20s widening rationale): this waits on an async call count, not
	// a precise timing relationship, so widening cannot mask the
	// regression -- failed twice on windows-latest CI (runs 31714546616
	// and 31718897797) at the original 5s bound.
	require.Eventually(t, func() bool {
		return coord.calls.Load() > coord.busyUntilCall
	}, 20*time.Second, 20*time.Millisecond,
		"coordinator must eventually be called past busyUntilCall — if this times out, the entry "+
			"was dead-lettered (deleted) before reaching that call count, meaning ErrCallQueuedNotExecuted "+
			"recoveries are still counting toward RunQueueMaxAttempts")

	require.Eventually(t, func() bool {
		pending, checkErr := svc.ListPendingRunQueueEntries(ctx)
		if checkErr != nil {
			return false
		}
		return len(pending) == 0
	}, 2*time.Second, 10*time.Millisecond, "entry should eventually be acked once the external owner frees up")
}
