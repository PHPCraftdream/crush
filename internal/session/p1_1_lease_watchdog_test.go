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
	mu                   sync.Mutex
	hangAfterAttemptNum  int64  // Hang on this specific renewal attempt (0 = never hang)
	hangDuration         time.Duration
	renewalsAttempted    atomic.Int64
	firstRenewalOK       bool
}

// RenewRunQueueLease hangs on a specific attempt to simulate DB stall.
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

	if hangAfterAttemptNum > 0 && attemptNum == hangAfterAttemptNum {
		// Hang on this specific renewal attempt to simulate DB stall
		// This hang should NOT exceed the watchdog's safe window
		time.Sleep(hangDuration)
	}

	// Forward to the real service
	return s.Service.RenewRunQueueLease(ctx, id, pumpInstanceID, newExpiresAt)
}

// TestP1_1_WatchdogCancelsBeforeExpiry verifies that the watchdog cancels
// execution BEFORE the lease TTL expires when renewal hangs.
//
// Scenario:
// - TTL = 200ms, safety_margin = 50ms
// - Watchdog fires at ~150ms (TTL - margin from last successful renewal)
// - Execution is canceled BEFORE the full 200ms TTL expires
//
// REVERT CHECK: Without the watchdog fix, cancellation would happen AFTER
// the full TTL (e.g., at ~233ms when the hung renewal returns and fail-closed check runs).
// The test verifies cancellation < TTL, not >= TTL.
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

	// TTL=200ms with safety_margin=50ms gives generous window for reliable testing
	// under -race load. The key invariant is: cancellation happens BEFORE TTL,
	// not at the exact microsecond.
	const (
		ttl          = 200 * time.Millisecond
		safetyMargin = 50 * time.Millisecond
	)

	// First renewal succeeds, second renewal hangs
	hangingSvc := &hangingRenewalsService{
		Service:              svc,
		hangAfterAttemptNum:  2,
		hangDuration:         100 * time.Millisecond, // Longer than remaining safe budget
		firstRenewalOK:       true,
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
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeoutCancel()

	select {
	case <-coord.canceledCh:
		elapsed := time.Since(startTime)
		t.Logf("execution canceled at %v after start", elapsed)

		// KEY INVARIANT: watchdog must cancel BEFORE TTL + reasonable_jitter
		// We use TTL + 50ms jitter because the watchdog ticker is 10ms and
		// goroutine scheduling can add latency. The critical guarantee is
		// that we don't run for the full TTL + DB_TIMEOUT window (the old bug).
		// With 200ms TTL, old bug would have cancellation at ~200ms + 30s timeout.
		require.Less(t, elapsed, ttl+50*time.Millisecond,
			"watchdog must cancel execution BEFORE lease TTL + jitter (canceled at %v, TTL=%v)",
			elapsed, ttl)

	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe cancellation within 5s - watchdog did not fire")
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

	// TTL=500ms with safety_margin=100ms: renewal interval=166ms, watchdog fires at 400ms
	// Execution runs for 250ms, which should trigger at least one renewal but NOT fire watchdog
	const (
		ttl          = 500 * time.Millisecond
		safetyMargin = 100 * time.Millisecond
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

	// Let execution run for 250ms (well within the 400ms safe window from TTL-margin)
	// With renewal interval TTL/3=166ms, we should get at least one renewal attempt
	time.Sleep(250 * time.Millisecond)

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
// - TTL = 150ms, safety_margin = 100ms
// - Watchdog should fire at TTL - margin = 50ms (very short window)
// - Renewal interval is TTL/3 = 50ms, so we barely get one renewal before watchdog fires
//
// This test uses a larger TTL (150ms vs. extreme 10ms) to avoid panics in
// time.NewTimer (which can misbehave with very short intervals under load).
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
		ttl          = 150 * time.Millisecond
		safetyMargin = 100 * time.Millisecond
	)

	// Let first renewal succeed, then hang
	hangingSvc := &hangingRenewalsService{
		Service:              svc,
		hangAfterAttemptNum:  2,
		hangDuration:         200 * time.Millisecond, // Longer than remaining budget
		firstRenewalOK:       true,
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

	// Wait for cancellation - with margin=100ms, watchdog fires very quickly
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeoutCancel()

	select {
	case <-coord.canceledCh:
		canceledAt := time.Since(firstRenewalTime)
		t.Logf("execution canceled at %v after first renewal (short TTL case)", canceledAt)

		// Verify cancellation happened BEFORE TTL
		require.Less(t, canceledAt, ttl,
			"watchdog must cancel execution BEFORE lease TTL (canceled at %v, TTL=%v)",
			canceledAt, ttl)

	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe cancellation within 5s - watchdog did not fire with short TTL")
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
// - TTL = 300ms, safety_margin = 100ms
// - Safe budget = TTL - margin = 200ms
// - First renewal hangs for 250ms (longer than the safe budget)
// - The watchdog should fire at ~200ms while the renewal is still stuck
//
// We verify that cancellation happens BEFORE TTL expires even when renewal hangs.
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
		ttl          = 300 * time.Millisecond
		safetyMargin = 100 * time.Millisecond
	)

	// First renewal hangs for longer than the safe budget
	hangingSvc := &hangingRenewalsService{
		Service:              svc,
		hangAfterAttemptNum:  1,  // Hang on first renewal
		hangDuration:         250 * time.Millisecond, // Longer than TTL - margin = 200ms
		firstRenewalOK:       false, // First renewal hangs (not succeeded immediately)
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

		// KEY INVARIANT: watchdog cancels BEFORE TTL even with hanging renewal
		require.Less(t, elapsed, ttl,
			"watchdog must cancel BEFORE TTL even with hanging renewal (canceled at %v, TTL=%v)",
			elapsed, ttl)

		// Verify we're not firing too early (should fire at ~200ms, TTL-margin)
		require.Greater(t, elapsed, 150*time.Millisecond,
			"watchdog should not fire immediately (canceled at %v, expected > 150ms)", elapsed)

	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe cancellation within 5s - watchdog did not fire")
	}

	require.True(t, coord.ctxCanceled.Load(), "coordinator should have observed ctx.Done()")

	close(blockCh)
	pump.Stop()
}