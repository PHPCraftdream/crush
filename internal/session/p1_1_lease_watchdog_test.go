// P1-1 regression test (docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md):
// RunQueuePump.executeEntry cancels execCtx BEFORE lease expiry via an independent
// watchdog timer with a safety margin.
//
// The bug: before the fix, when RenewRunQueueLease hung (e.g., DB stall for the
// full 30s timeout), the executor would continue running for TTL + timeout (e.g.,
// 30s + 30s = 60s total) even though the lease expired at TTL (30s). During those
// extra ~30s, another pump instance could legitimately take ownership and start
// a duplicate execution. The fail-closed check (time.Since(lastSuccessfulRenewal) >= TTL)
// only ran BEFORE the DB call and repeated AFTER it returned, so it couldn't catch
// this case.
//
// The fix:
// 1. Add an independent watchdog timer (separate goroutine from renewal loop)
// 2. Watchdog fires at: time_of_last_renewal + TTL - safety_margin
// 3. Watchdog cancels execCtx when it fires, BEFORE another pump can take ownership
// 4. Each renewal gets a timeout = remaining safe budget (not fixed 30s)
//
// SEMANTICS: The watchdog provides a strong bound against double-execution even
// when DB stalls. Residual window is safety_margin + provider/tool cancellation latency,
// not the full TTL.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go, disable the watchdog by setting its timer deadline
//     to TTL (instead of TTL - safety_margin) or removing the watchdog goroutine
//  2. This test will FAIL with the predicted symptom (cancellation happens AFTER
//     the full TTL + DB timeout window, not before), verifying the fix is needed.

package session_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// hangingRenewalsService wraps session.Service and can hang RenewRunQueueLease calls
// to simulate DB stalls.
type hangingRenewalsService struct {
	session.Service
	mu                  sync.Mutex
	hangAfterAttemptNum int64 // Hang starting from this renewal attempt (0 = never hang)
	hangDuration        time.Duration
	renewalsAttempted   atomic.Int64
	firstRenewalOK      bool
}

// RenewRunQueueLease hangs starting from a specific attempt, to simulate a
// sustained DB stall.
//
// P0-3 fix note: this used to hang on ONLY the exact hangAfterAttemptNum
// attempt, then forward every later attempt straight through. That was
// sufficient before P0-3: the watchdog's old post-call-time tracking meant
// a single hung-then-recovered renewal cycle was enough to trip the
// fail-closed timeout reliably within the test's tight timing window. Once
// the watchdog is correctly DB-anchored (this fix), a single hang followed
// by healthy renewals lets the pump's OWN renewal loop legitimately
// self-heal — a later successful renewal simply extends the tracked
// deadline again, exactly as it's supposed to for a real recovered DB, so
// the watchdog may never fire at all if hangAfterAttemptNum's one bad
// attempt is followed by good ones (observed directly: intermittent "did
// not fire within Ns" test timeouts once TTLs were scaled up per P0-3 to
// accommodate lease_expires_at's whole-seconds granularity, since scaling
// up TTL also gives more real-time room for a later attempt to land and
// recover before the watchdog's deadline). Hanging on every attempt from
// hangAfterAttemptNum onward (not just the one) is what actually simulates
// "the DB stays stuck," which is the scenario these tests are meant to
// exercise — a transient single-attempt hiccup, which the old exact-match
// behavior actually models, is a materially different (and already
// separately covered, e.g. TestP1_1_FastRenewalNoFalsePositive) scenario.
func (s *hangingRenewalsService) RenewRunQueueLease(ctx context.Context, id, pumpInstanceID string, newExpiresAt int64) (bool, error) {
	attemptNum := s.renewalsAttempted.Add(1)

	s.mu.Lock()
	hangAfterAttemptNum := s.hangAfterAttemptNum
	hangDuration := s.hangDuration
	firstRenewalOK := s.firstRenewalOK
	s.mu.Unlock()

	if firstRenewalOK && attemptNum == 1 {
		// First renewal: allow it to succeed (this sets lastSuccessfulRenewal)
		return s.Service.RenewRunQueueLease(ctx, id, pumpInstanceID, newExpiresAt)
	}

	if hangAfterAttemptNum > 0 && attemptNum >= hangAfterAttemptNum {
		// Hang on this and every subsequent renewal attempt to simulate a
		// sustained DB stall (not a one-off hiccup that later recovers).
		select {
		case <-time.After(hangDuration):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}

	// Forward to the real service
	return s.Service.RenewRunQueueLease(ctx, id, pumpInstanceID, newExpiresAt)
}

// TestP1_1_WatchdogCancelsBeforeExpiry verifies that the watchdog cancels
// execution BEFORE the lease TTL expires when renewal hangs.
//
// Scenario:
// - TTL = 5s, safety_margin = 1.25s
// - Watchdog fires at ~3.75s (TTL - margin from last successful renewal)
// - Execution is canceled BEFORE the full 5s TTL expires
//
// REVERT CHECK: Without the watchdog fix, cancellation would happen AFTER
// the full TTL (e.g., at ~TTL + DB-timeout when the hung renewal returns
// and fail-closed check runs). The test verifies cancellation is well
// before the naive "full TTL + hang duration" bound, not >= it.
//
// P0-3 fix note: TTL/margin/hangDuration were scaled up 25x from this
// test's original 200ms/50ms/100ms. See TestP1_1_DynamicRenewalTimeout's
// P0-3 fix note (below) for why: lease_expires_at's whole-Unix-seconds
// ceiling rounding (the only safe rounding direction once the watchdog is
// correctly DB-anchored, per P0-3) adds up to ~1s of slack that dominated
// timing at this test's original sub-second scale.
func TestP1_1_WatchdogCancelsBeforeExpiry(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-1-watchdog")
	ctx := t.Context()

	idempotencyKey := "p1-1-watchdog-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that will block and observe context cancellation
	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	// TTL=5s with safety_margin=1.25s gives generous window for reliable
	// testing under -race load, and enough headroom above
	// lease_expires_at's ~1s ceiling-rounding slack (see this test's P0-3
	// fix note above). The key invariant is: cancellation happens BEFORE
	// TTL, not at the exact millisecond.
	const (
		ttl          = 5 * time.Second
		safetyMargin = 1250 * time.Millisecond
	)

	// First renewal succeeds, second renewal hangs
	hangingSvc := &hangingRenewalsService{
		Service:             svc,
		hangAfterAttemptNum: 2,
		hangDuration:        2500 * time.Millisecond, // Longer than remaining safe budget
		firstRenewalOK:      true,
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:                      hangingSvc,
		Coordinator:                   coord,
		PumpInstanceID:                "p1-1-watchdog-pump",
		TestTick:                      func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:                  ttl,
		TestLeaseWatchdogSafetyMargin: safetyMargin,
	})
	pump.Start()

	// Wait for the coordinator to be called (worker started)
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond,
		"coordinator must be called (worker started)")

	// Record start time for upper bound check
	startTime := time.Now()

	// Wait for coordinator to observe cancellation
	// With watchdog, cancellation should happen BEFORE TTL expires
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer timeoutCancel()

	select {
	case <-coord.canceledCh:
		elapsed := time.Since(startTime)
		t.Logf("execution canceled at %v after start", elapsed)

		// KEY INVARIANT: watchdog must cancel reasonably close to TTL, not
		// run for the full TTL + a large DB timeout window (the old bug —
		// with 5s TTL, that bug would show up as cancellation well past
		// 5s + a much larger DB timeout). The bound covers BOTH
		// lease_expires_at's ~1s whole-Unix-seconds ceiling-rounding slack
		// (see this test's P0-3 fix note above — the only safe rounding
		// direction once the watchdog is correctly DB-anchored) AND real
		// scheduling/DB contention jitter observed when this whole package
		// runs its many concurrent-pump tests together under -race.
		//
		// TTL+5s margin, kept after an empirical investigation prompted by
		// independent review (docs/reviews/2026-08-13-release-readiness-
		// static-audit.md, finding M7): the review correctly points out
		// that TTL+5s is loose enough to no longer distinguish "watchdog
		// correctly DB-anchored" from "watchdog still computing its
		// deadline from post-call time" (the exact P0-3 bug) for any
		// renewal call slower than ~4s. Tightening was tried empirically
		// rather than picked by feel: TTL+2s flaked ~5% (2/40 isolated
		// -race runs on this machine), TTL+3s flaked ~7% (2/30) — BOTH
		// from real scheduling/DB contention when this package's many
		// concurrent-pump tests run together under -race, confirmed via
		// the actual overshoot magnitudes (up to ~3s past the bound on the
		// failing runs), not a marginal few-millisecond miss that a
		// slightly wider margin would obviously fix. A materially tighter
		// bound on THIS specific wall-clock-elapsed assertion trades a
		// real, measurable CI flake rate for a theoretical regression-
		// detection improvement that a separate, dedicated, non-flaky test
		// (TestP0_3_WatchdogUsesAbsoluteDBExpiryNotPostCallTime, which
		// compares against the actual DB-committed expiry directly instead
		// of a wall-clock nominal bound) already covers precisely. Kept at
		// TTL+5s on that basis — a genuinely tighter version of this
		// specific test would need to compare against the real
		// lease_expires_at the way that dedicated test does, not just a
		// smaller constant added to wall-clock elapsed time.
		require.Less(t, elapsed, ttl+5*time.Second,
			"watchdog must cancel execution well before lease TTL + old-bug-sized jitter (canceled at %v, TTL=%v)",
			elapsed, ttl)

	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe cancellation within 15s - watchdog did not fire")
	}

	// Verify ctxCanceled flag is set
	require.True(t, coord.ctxCanceled.Load(), "coordinator should have observed ctx.Done()")

	// Unblock and let pump finish cleanly
	close(blockCh)
	pump.Stop()
}

// TestP1_1_FastRenewalNoFalsePositive verifies that normal, fast renewals
// do NOT trigger watchdog cancellation. The watchdog should only fire when
// renewal is actually stalled, not when everything is working normally.
//
// Scenario:
// - TTL = 2s, safety_margin = 500ms
// - All renewals succeed quickly
// - Execution runs for 1s (well within the safe window)
// - No watchdog cancellation should occur
//
// This is a timing-stable test with generous margins to avoid flakiness
// under -race load (based on lessons from P1-2 tests, where TTL had to be
// expanded from 200-500ms to 2s).
func TestP1_1_FastRenewalNoFalsePositive(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-1-false-positive")
	ctx := t.Context()

	idempotencyKey := "p1-1-false-positive-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that will block for a controlled duration
	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	// TTL=2s with safety_margin=500ms: renewal interval=666ms, watchdog fires at 1500ms.
	// Execution runs for 800ms below (>1 renewal interval, so at least one renewal
	// attempt is guaranteed) while leaving ~700ms of headroom before the watchdog
	// would fire — matches the doc comment's stated scenario. The previous
	// ttl=500ms/margin=100ms (400ms safe budget, only 150ms headroom against a
	// 250ms sleep) was confirmed flaky under -race load: this is the only
	// regression guard for the watchdog's false-positive-cancels-healthy-work
	// failure mode, so tight margins here are worse than on an ordinary timing
	// test (found and fixed after an independent review flagged the doc-vs-code
	// mismatch and reproduced the flake).
	const (
		ttl          = 2 * time.Second
		safetyMargin = 500 * time.Millisecond
	)

	// All renewals succeed immediately (no hang)
	hangingSvc := &hangingRenewalsService{
		Service:        svc,
		firstRenewalOK: true,
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:                      hangingSvc,
		Coordinator:                   coord,
		PumpInstanceID:                "p1-1-false-positive-pump",
		TestTick:                      func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:                  ttl,
		TestLeaseWatchdogSafetyMargin: safetyMargin,
	})
	pump.Start()

	// Wait for the coordinator to be called
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Let execution run for 800ms (well within the 1500ms safe window from TTL-margin)
	// With renewal interval TTL/3=666ms, we should get at least one renewal attempt
	time.Sleep(800 * time.Millisecond)

	// Verify coordinator did NOT observe cancellation
	// The watchdog should NOT fire because renewals are working normally
	require.False(t, coord.ctxCanceled.Load(),
		"coordinator should NOT observe cancellation with fast renewals")

	// Unblock and let execution finish normally
	close(blockCh)

	// Wait for execution to complete
	select {
	case <-coord.completedCh:
		// Expected: execution completed normally
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not complete within 5s")
	}

	// Stop the pump
	pump.Stop()

	// Verify at least one renewal was attempted (showing renewal loop was active)
	require.GreaterOrEqual(t, hangingSvc.renewalsAttempted.Load(), int64(1),
		"at least 1 renewal should have been attempted during execution")
}

// TestP1_1_WatchdogWithVeryShortTTL verifies the watchdog works correctly
// even with very short TTLs (edge case testing). This ensures the watchdog
// timer doesn't panic or misbehave when TTL < safety_margin.
//
// Scenario:
// - TTL = 3s, safety_margin = 2s
// - Watchdog should fire at TTL - margin = 1s after the first renewal
// - Renewal interval is TTL/3 = 1s, so we barely get one renewal before watchdog fires
//
// P0-3 fix note: TTL/margin/hangDuration were scaled up 20x from this
// test's original 150ms/100ms/200ms. See TestP1_1_DynamicRenewalTimeout's
// P0-3 fix note (above) for why: lease_expires_at's whole-Unix-seconds
// ceiling rounding (the only safe rounding direction once the watchdog is
// correctly anchored to what's actually persisted, per P0-3) adds up to
// ~1s of slack that dominates timing at sub-second TTL/margin scales.
func TestP1_1_WatchdogWithVeryShortTTL(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-1-short-ttl")
	ctx := t.Context()

	idempotencyKey := "p1-1-short-ttl-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that will block
	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	const (
		ttl          = 3 * time.Second
		safetyMargin = 2 * time.Second
	)

	// Let first renewal succeed, then hang
	hangingSvc := &hangingRenewalsService{
		Service:             svc,
		hangAfterAttemptNum: 2,
		hangDuration:        4 * time.Second, // Longer than remaining budget
		firstRenewalOK:      true,
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:                      hangingSvc,
		Coordinator:                   coord,
		PumpInstanceID:                "p1-1-short-ttl-pump",
		TestTick:                      func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:                  ttl,
		TestLeaseWatchdogSafetyMargin: safetyMargin,
	})
	pump.Start()

	// Wait for the coordinator to be called
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Wait for the first renewal to succeed
	require.Eventually(t, func() bool {
		return hangingSvc.renewalsAttempted.Load() >= 1
	}, 5*time.Second, 20*time.Millisecond)

	firstRenewalTime := time.Now()

	// Wait for cancellation - with margin=2s, watchdog fires quickly
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer timeoutCancel()

	select {
	case <-coord.canceledCh:
		canceledAt := time.Since(firstRenewalTime)
		t.Logf("execution canceled at %v after first renewal (short TTL case)", canceledAt)

		// Verify cancellation happened reasonably close to TTL — allowing
		// for lease_expires_at's whole-seconds ceiling rounding (up to ~1s
		// of extra slack, see this test's P0-3 fix note above).
		require.Less(t, canceledAt, ttl+1200*time.Millisecond,
			"watchdog must cancel close to lease TTL (canceled at %v, TTL=%v)",
			canceledAt, ttl)

	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe cancellation within 8s - watchdog did not fire with short TTL")
	}

	// Verify ctxCanceled flag is set
	require.True(t, coord.ctxCanceled.Load(), "coordinator should have observed ctx.Done()")

	// Unblock and stop
	close(blockCh)
	pump.Stop()
}

// TestP1_1_DynamicRenewalTimeout verifies that each renewal gets a timeout
// equal to the remaining safe lease budget (not a fixed 30s).
//
// Scenario:
// - TTL = 3s, safety_margin = 1s
// - Safe budget = TTL - margin = 2s
// - First renewal hangs for 2.5s (longer than the safe budget)
// - The watchdog should fire at ~2s while the renewal is still stuck
//
// We verify that cancellation happens BEFORE TTL expires even when renewal hangs.
//
// P0-3 fix note: TTL/margin were scaled up 10x from this test's original
// 300ms/100ms (with hang 250ms). lease_expires_at is a whole-Unix-seconds
// column; making the watchdog correctly DB-anchored (P0-3's whole point —
// firing based on what's actually persisted, not a locally-tracked
// wall-clock timestamp that can drift from it) means the column's rounding
// is now load-bearing for the watchdog's own timing. Rounding UP
// (ceilUnixSeconds in session.go) is the only safe direction — rounding
// down can silently eat an entire short safety margin, causing the
// watchdog to fire almost immediately depending on wall-clock phase alone
// (see run_queue_pump.go's P0-3 fix note) — but ceiling means the
// persisted (and therefore watchdog-tracked) deadline can land up to ~1s
// later than the nominal value. At the original 300ms/100ms scale, that
// ~1s of unavoidable rounding slack was comparable to or larger than the
// entire TTL, making this test's tight "fires within ~50ms of TTL-margin"
// assertions structurally impossible to satisfy reliably. At this test's
// new 10x scale, ~1s of slack is a small, tolerable fraction of a 3s TTL.
func TestP1_1_DynamicRenewalTimeout(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-1-dynamic-timeout")
	ctx := t.Context()

	idempotencyKey := "p1-1-dynamic-timeout-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that will block
	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	const (
		ttl          = 3 * time.Second
		safetyMargin = 1 * time.Second
	)

	// First renewal hangs for longer than the safe budget
	hangingSvc := &hangingRenewalsService{
		Service:             svc,
		hangAfterAttemptNum: 1,                       // Hang on first renewal
		hangDuration:        2500 * time.Millisecond, // Longer than TTL - margin = 2s
		firstRenewalOK:      false,                   // First renewal hangs (not succeeded immediately)
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:                      hangingSvc,
		Coordinator:                   coord,
		PumpInstanceID:                "p1-1-dynamic-timeout-pump",
		TestTick:                      func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:                  ttl,
		TestLeaseWatchdogSafetyMargin: safetyMargin,
	})
	pump.Start()

	// Wait for the coordinator to be called
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	startTime := time.Now()

	// Wait for cancellation
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeoutCancel()

	select {
	case <-coord.canceledCh:
		elapsed := time.Since(startTime)
		t.Logf("execution canceled at %v after start", elapsed)

		// KEY INVARIANT: watchdog cancels while renewal is still hung, well
		// before it would have hung long enough to matter, and — allowing
		// for lease_expires_at's whole-seconds ceiling rounding (up to ~1s
		// of extra slack, see the P0-3 fix note above this test) — not
		// wildly later than the nominal TTL either.
		require.Less(t, elapsed, ttl+1200*time.Millisecond,
			"watchdog must cancel reasonably close to TTL even with hanging renewal (canceled at %v, TTL=%v)",
			elapsed, ttl)

		// Verify we're not firing too early (should fire at ~2s, TTL-margin,
		// possibly later due to ceiling rounding, but not much earlier).
		require.Greater(t, elapsed, 1500*time.Millisecond,
			"watchdog should not fire immediately (canceled at %v, expected > 1.5s)", elapsed)

	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe cancellation within 5s - watchdog did not fire")
	}

	require.True(t, coord.ctxCanceled.Load(), "coordinator should have observed ctx.Done()")

	close(blockCh)
	pump.Stop()
}
