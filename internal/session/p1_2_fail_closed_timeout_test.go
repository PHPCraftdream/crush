// P1-2 regression test (docs/reviews/2026-08-12-post-fix-release-readiness-review.md):
// RunQueuePump.executeEntry cancels execCtx when renewal fails continuously
// for longer than the lease TTL (fail-closed timeout).
//
// The bug: before the fix, when RenewRunQueueLease returned err != nil (not ok=false),
// the renewal loop only logged a warning and continued. If renewal kept failing for > TTL,
// another process could clean up the lease via CleanupExpiredLeases and start a second
// execution. The first executor would only learn about this on a LATER successful renewal
// that returned !ok — by then, both executors had been running in parallel, performing
// real work (LLM calls, tool execution, message writes).
//
// The fix:
// 1. Track lastSuccessfulRenewal time.Time, initialized at execution start
// 2. Before each renewal attempt, check if time.Since(lastSuccessfulRenewal) >= leaseTTL
// 3. If true, cancel execCtx and set leaseLost (same path as explicit !ok)
// 4. This provides fail-closed behavior: execution stops even without an explicit
//    "lease lost" signal from the database
//
// SEMANTICS: This fix provides AT-LEAST-ONCE semantics for persistent side effects,
// not exactly-once. The residual overlap window is from "renewal stops responding" until
// "fail-closed timeout fires and execCancel() propagates to Coordinator.Run". A full
// fencing token (checked before each persistent write) is required for strict exactly-once.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go, remove the fail-closed timeout check (the
//     timeSinceLastSuccess >= p.leaseTTL() block before the renewal call)
//  2. Keep the explicit !ok case (P1-2 previous round)
//  3. This test will TIMEOUT/FALSE_ASSERT with the predicted symptom (execution continues
//     indefinitely despite continuous renewal errors, coordinator never observes
//     cancellation within the expected window).

package session_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// blockingRenewalsService wraps session.Service and can block/cause errors on RenewRunQueueLease
type blockingRenewalsService struct {
	session.Service
	mu                sync.Mutex
	renewalErrorCount int          // Number of consecutive renewals to return errors
	renewalsAttempted atomic.Int64 // Counter for renewal attempts
	firstRenewalOK    bool         // Whether the first renewal should succeed (sets lastSuccessfulRenewal)
}

// RenewRunQueueLease returns errors for the first N calls (or all after first), then succeeds.
func (s *blockingRenewalsService) RenewRunQueueLease(ctx context.Context, id, pumpInstanceID string, newExpiresAt int64) (bool, error) {
	s.mu.Lock()
	renewalErrorCount := s.renewalErrorCount
	firstRenewalOK := s.firstRenewalOK
	s.mu.Unlock()

	attemptNum := s.renewalsAttempted.Add(1)

	if firstRenewalOK && attemptNum == 1 {
		// First renewal: allow it to succeed (this sets lastSuccessfulRenewal)
		return s.Service.RenewRunQueueLease(ctx, id, pumpInstanceID, newExpiresAt)
	}

	if renewalErrorCount > 0 && attemptNum <= int64(renewalErrorCount) {
		// Return an error for the first N renewals
		return false, sql.ErrConnDone
	}

	// Forward to the real service
	return s.Service.RenewRunQueueLease(ctx, id, pumpInstanceID, newExpiresAt)
}

// TestP1_2_FailClosedTimeoutOnRenewalErrors verifies that when renewal
// fails continuously for longer than the lease TTL, the executor cancels
// itself via fail-closed timeout (even without an explicit !ok response).
//
// REVERT CHECK: Without the fix, execution continues indefinitely despite
// continuous renewal errors — the coordinator never observes cancellation
// and the test times out/fails.
func TestP1_2_FailClosedTimeoutOnRenewalErrors(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-2-fail-closed")
	ctx := t.Context()

	idempotencyKey := "p1-2-fail-closed-probe"
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

	// Use a very short lease TTL (100ms) so the fail-closed timeout
	// fires quickly. The renewal interval is TTL/3 ≈ 33ms.
	// Block ALL renewals after the first succeeds.
	blockingSvc := &blockingRenewalsService{
		Service:           svc,
		renewalErrorCount: 100,  // Effectively infinite errors
		firstRenewalOK:    true, // First renewal succeeds to set lastSuccessfulRenewal
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       blockingSvc,
		Coordinator:    coord,
		PumpInstanceID: "p1-2-fail-closed-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for the coordinator to be called (meaning the worker is in-flight)
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond,
		"coordinator must be called (worker started)")

	// Wait for the first renewal to succeed (sets lastSuccessfulRenewal)
	require.Eventually(t, func() bool {
		return blockingSvc.renewalsAttempted.Load() >= 1
	}, 5*time.Second, 20*time.Millisecond,
		"first renewal must be called")

	// Now all subsequent renewals will fail. The fail-closed timeout should fire
	// within TTL (100ms) after the last successful renewal.
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeoutCancel()

	// Wait for the coordinator to observe context cancellation
	// This should happen within ~100-150ms (TTL + one renewal tick)
	select {
	case <-coord.canceledCh:
		// Expected: coordinator observed context cancellation due to fail-closed timeout
	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe context cancellation within 5s - fail-closed timeout not working")
	}

	// Verify that ctxCanceled flag is set
	require.True(t, coord.ctxCanceled.Load(), "coordinator should have observed ctx.Done()")

	// Verify that multiple renewals were attempted (and failed)
	require.GreaterOrEqual(t, blockingSvc.renewalsAttempted.Load(), int64(2),
		"at least one renewal should have failed after the first success")

	// Verify the timeout happened within a reasonable window (not too early, not too late)
	// With TTL=100ms and renewal interval=33ms, we expect cancellation between 100-150ms
	// We've already waited for the first renewal, so total elapsed should be roughly
	// TTL + a bit of scheduling delay.
	require.Less(t, blockingSvc.renewalsAttempted.Load(), int64(10),
		"fail-closed timeout should fire quickly, not after many failed renewals")

	// Stop the pump
	pump.Stop()
}

// TestP1_2_SingleRenewalErrorDoesNotTriggerTimeout verifies that a
// single renewal error (err != nil) does NOT trigger the fail-closed
// timeout. Only CONTINUOUS errors beyond TTL should trigger it.
func TestP1_2_SingleRenewalErrorDoesNotTriggerTimeout(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-2-single-error")
	ctx := t.Context()

	idempotencyKey := "p1-2-single-error-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that blocks for a bit (giving renewal loop time to fire multiple times)
	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	// Block ONLY the second renewal (allow all others to succeed)
	blockingSvc := &blockingRenewalsService{
		Service:           svc,
		renewalErrorCount: 1, // Block only one renewal
		firstRenewalOK:    true,
	}

	// TTL=2s (tick interval TTL/3≈666ms) gives generous slack against
	// scheduling jitter under load: even if a renewal tick is delayed by a
	// few hundred ms (observed to happen under heavy parallel test-suite
	// contention — this test flaked intermittently at TTL=500ms/interval
	// ~166ms, where jitter of just ~150-200ms could push
	// time.Since(lastSuccessfulRenewal) past the fail-closed threshold
	// before the next renewal had a chance to refresh it), there's still
	// well over a second of margin before a false-positive cancellation.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       blockingSvc,
		Coordinator:    coord,
		PumpInstanceID: "p1-2-single-error-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   2 * time.Second,
	})
	pump.Start()

	// Wait for the coordinator to be called
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Wait for at least 2 renewals to be attempted (one success, one error, then more successes)
	require.Eventually(t, func() bool {
		return blockingSvc.renewalsAttempted.Load() >= 2
	}, 5*time.Second, 20*time.Millisecond,
		"at least 2 renewals should have been attempted")

	// Wait a bit more to ensure no fail-closed timeout fires.
	// With TTL=2s and renewal interval≈666ms, we're well within the TTL window.
	time.Sleep(200 * time.Millisecond)

	// The coordinator should NOT have observed cancellation
	// (single error should not trigger fail-closed timeout)
	require.False(t, coord.ctxCanceled.Load(),
		"coordinator should NOT observe cancellation after a single renewal error")

	// Unblock and let execution finish normally
	close(blockCh)

	// Wait for execution to complete
	select {
	case <-coord.completedCh:
		// Expected: execution completed normally
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not complete within 5s")
	}

	pump.Stop()

	// Verify multiple renewals were attempted (one blocked, others succeeded)
	require.GreaterOrEqual(t, blockingSvc.renewalsAttempted.Load(), int64(2),
		"at least 2 renewals should have been attempted")
}

// TestP1_2_FailClosedTimeoutWindow verifies that the fail-closed timeout
// fires after a full TTL has elapsed since the last successful renewal,
// not sooner. This ensures we don't have false positives on transient
// renewal failures.
func TestP1_2_FailClosedTimeoutWindow(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-2-timeout-window")
	ctx := t.Context()

	idempotencyKey := "p1-2-timeout-window-probe"
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

	// Block ALL renewals after the first succeeds
	blockingSvc := &blockingRenewalsService{
		Service:           svc,
		renewalErrorCount: 100,
		firstRenewalOK:    true,
	}

	// TTL=2s (vs. an earlier 200ms) gives a much wider floor-check margin.
	// At 200ms, this test flaked intermittently under heavy parallel
	// test-suite load: startTime was captured after confirming
	// coord.entryCount.Load() > 0, but the SUT's own lastSuccessfulRenewal
	// clock actually starts ticking earlier (when the renewal goroutine
	// launches, before the coordinator callback and its atomic counter
	// become externally observable) — under scheduling jitter that gap
	// could itself approach the 150ms floor, making a perfectly-correct
	// fail-closed cancellation look like it fired "too early" relative to
	// startTime. Capturing startTime right after the first renewal is
	// confirmed (closer to the SUT's actual clock origin) plus a much
	// bigger TTL closes the gap.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       blockingSvc,
		Coordinator:    coord,
		PumpInstanceID: "p1-2-timeout-window-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   2 * time.Second,
	})
	pump.Start()

	// Wait for the coordinator to be called
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Wait for the first renewal to succeed, then start measuring from here
	// — as close as achievable to the SUT's own lastSuccessfulRenewal
	// timestamp.
	require.Eventually(t, func() bool {
		return blockingSvc.renewalsAttempted.Load() >= 1
	}, 5*time.Second, 20*time.Millisecond)
	startTime := time.Now()

	// The coordinator should NOT observe cancellation within the first TTL window
	// (renewals should still be succeeding during this time... wait, we're blocking them)
	// Actually, since we're blocking renewals, the coordinator WILL observe cancellation
	// after TTL. Let's just verify it doesn't cancel TOO early (e.g., well before the 2s TTL)
	select {
	case <-coord.canceledCh:
		elapsed := time.Since(startTime)
		if elapsed < 1500*time.Millisecond {
			t.Fatalf("coordinator observed cancellation too early (after %v), expected at least ~2s (TTL)",
				elapsed)
		}
		// Expected: cancellation after TTL
	case <-time.After(8 * time.Second):
		t.Fatal("coordinator did not observe cancellation within 8s")
	}

	// Stop the pump
	pump.Stop()
}
