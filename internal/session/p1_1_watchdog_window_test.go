// P1-1 regression test (docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md):
// RunQueuePump.executeEntry cancels execCtx BEFORE lease expiry via the
// independent watchdog timer with a safety margin.
//
// This test verifies the NEW contract introduced by P1-1: cancellation happens
// at TTL - safety_margin, NOT at TTL. The old fail-closed timeout (which fired
// at TTL) has been removed because the watchdog always fires first.
//
// SEMANTICS: The durable queue provides AT-LEAST-ONCE guarantees for
// persistent side effects (LLM calls, tool execution, message writes),
// not exactly-once. The watchdog MINIMIZES but does NOT GUARANTEE
// elimination of all duplicate-execution windows.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go, restore the old fail-closed timeout check
//     (the timeSinceLastSuccess >= p.leaseTTL() block)
//  2. Remove or disable the watchdog goroutine
//  3. This test will FAIL with the predicted symptom (cancellation happens
//     after ~TTL, not at TTL - margin).

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

// TestP1_1_WatchdogCancelsAtTTLMinusMargin verifies that the watchdog
// fires at TTL - safety_margin, not at TTL.
//
// Scenario:
// - TTL = 10s, safety_margin = 2.5s
// - First renewal succeeds, then all subsequent renewals fail
// - Watchdog should fire at ~7.5s (TTL - margin), not at 10s
//
// REVERT CHECK: Without the watchdog fix (or with old fail-closed timeout),
// cancellation would happen at ~10s (TTL), not at ~7.5s.
//
// P0-3 fix note: TTL/margin were scaled up 5x from this test's original
// 2s/500ms. See TestP1_1_DynamicRenewalTimeout's P0-3 fix note (in
// p1_1_lease_watchdog_test.go) for why: lease_expires_at's whole-Unix-
// seconds ceiling rounding (the only safe rounding direction once the
// watchdog is correctly DB-anchored, per P0-3) adds up to ~1s of slack
// that was a large fraction of the original 300ms window this test
// asserted on; at 5x scale it's a small, tolerable fraction.
func TestP1_1_WatchdogCancelsAtTTLMinusMargin(t *testing.T) {
	t.Parallel()

	sess, svc, _ := setupTestSessionWithDB(t, "test-session-p1-1-watchdog-window")
	ctx := t.Context()

	idempotencyKey := "p1-1-watchdog-window-probe"
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

	// TTL=10s, safety_margin=2.5s: watchdog fires at ~7.5s
	// Block ALL renewals after the first succeeds
	blockingSvc := &blockingRenewalsService{
		Service:           svc,
		renewalErrorCount: 100,
		firstRenewalOK:    true,
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:                      blockingSvc,
		Coordinator:                   coord,
		PumpInstanceID:                "p1-1-watchdog-window-pump",
		TestTick:                      func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:                  10 * time.Second,
		TestLeaseWatchdogSafetyMargin: 2500 * time.Millisecond,
	})
	pump.Start()

	// Wait for the coordinator to be called
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Wait for the first renewal to succeed, then start measuring from here
	require.Eventually(t, func() bool {
		return blockingSvc.renewalsAttempted.Load() >= 1
	}, 5*time.Second, 20*time.Millisecond)
	startTime := time.Now()

	// Wait for the coordinator to observe context cancellation
	// With watchdog, cancellation should happen at ~7.5s (TTL - margin)
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer timeoutCancel()

	select {
	case <-coord.canceledCh:
		elapsed := time.Since(startTime)
		t.Logf("execution canceled at %v after first renewal", elapsed)

		// KEY INVARIANT: watchdog cancels at TTL - margin, not TTL
		// With TTL=10s and margin=2.5s, expected cancellation at ~7.5s.
		// Generous bounds account for scheduling jitter AND up to ~1s of
		// lease_expires_at ceiling-rounding slack (see this test's P0-3 fix
		// note above).
		require.Less(t, elapsed, 9*time.Second,
			"watchdog must cancel well before TTL (canceled at %v, TTL=10s, margin=2.5s)",
			elapsed)
		require.Greater(t, elapsed, 6*time.Second,
			"watchdog should not cancel too early (canceled at %v, TTL=10s, margin=2.5s)",
			elapsed)

	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe context cancellation within 15s - watchdog did not fire")
	}

	// Verify ctxCanceled flag is set
	require.True(t, coord.ctxCanceled.Load(), "coordinator should have observed ctx.Done()")

	// Unblock and stop
	close(blockCh)
	pump.Stop()
}
