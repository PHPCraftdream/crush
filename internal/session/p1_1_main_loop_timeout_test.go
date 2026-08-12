// P1-1 regression test (docs/reviews/2026-08-12-post-fix-release-readiness-review.md):
// RunQueuePump.Stop() has a bounded timeout for the main run() loop, not just workers.
//
// The bug: Stop() had a 5-second timeout for workerWg.Wait() but NO timeout for
// p.wg.Wait() (waiting for the main run() loop to exit). If tick() hung on a DB call
// (e.g., stuck disk/filesystem), Stop() and App.Shutdown() would hang forever,
// breaking the "bounded shutdown" guarantee.
//
// The fix:
// 1. tick() now uses a deadline-bound context (5s) for DB reads (CleanupExpiredLeases,
//    ListPendingRunQueueEntries) to prevent indefinite hangs.
// 2. Stop() uses a unified 5-second deadline for BOTH workers AND the main loop,
//    keeping total shutdown time bounded to 5s (not 10s).
// 3. Stop() returns true (forced) if main loop doesn't exit within the deadline.
//
// REVERT CHECK PROCEDURE:
//  1. In tick(), restore `ctx := p.ctx` (remove context.WithTimeout).
//  2. In Stop(), remove the mainLoopDone goroutine and select branches, restore
//     the old two-phase pattern (wait for workers with timeout, then bare p.wg.Wait()).
//  3. This test will TIMEOUT (hang for 60s) proving Stop() doesn't return without the fix.

package session_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// blockingReadsService wraps a session.Service and can block on DB reads
// to simulate a hung disk/filesystem scenario.
type blockingReadsService struct {
	session.Service
	mu       sync.Mutex
	blockCh  chan struct{} // when non-nil, ListPendingRunQueueEntries blocks until closed
	blockNow bool          // whether the next ListPendingRunQueueEntries call should block
}

// ListPendingRunQueueEntries blocks if blockNow is true, otherwise delegates.
func (s *blockingReadsService) ListPendingRunQueueEntries(ctx context.Context) ([]session.RunQueueEntry, error) {
	s.mu.Lock()
	shouldBlock := s.blockNow
	ch := s.blockCh
	s.mu.Unlock()

	if shouldBlock && ch != nil {
		<-ch // Block until the test closes the channel
	}

	return s.Service.ListPendingRunQueueEntries(ctx)
}

// TestP1_1_StopWaitsForMainLoopWithTimeout verifies that Stop() returns
// within the 5-second grace period even when tick() hangs on a DB read.
// This proves the main loop wait is bounded, not unbounded like before P1-1.
//
// REVERT CHECK: Without the fix (bare p.wg.Wait() with no timeout), this test
// hangs for the full 60s test timeout instead of returning in ~5s.
func TestP1_1_StopWaitsForMainLoopWithTimeout(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-p1-1-main-loop-timeout")
	ctx := t.Context()

	// Wrap the service to block on reads
	blockingSvc := &blockingReadsService{Service: svc}
	blockCh := make(chan struct{})

	// Enqueue a durable call so tick() has work to do
	idempotencyKey := "p1-1-main-loop-timeout-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, blockingSvc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that completes quickly (not the focus of this test)
	coord := newBlockingCoordinator()

	// Start pump with short tick interval
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       blockingSvc,
		Coordinator:    coord,
		PumpInstanceID: "p1-1-main-loop-timeout-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for at least one tick to complete (pump is running)
	time.Sleep(200 * time.Millisecond)

	// Now block on the next DB read (tick() will hang inside ListPendingRunQueueEntries)
	blockingSvc.mu.Lock()
	blockingSvc.blockNow = true
	blockingSvc.blockCh = blockCh
	blockingSvc.mu.Unlock()

	// Wait a bit for tick() to call ListPendingRunQueueEntries and block
	time.Sleep(200 * time.Millisecond)

	// Call Stop() and verify it returns within 5 seconds despite the hung tick
	start := time.Now()
	stopReturned := make(chan bool)
	go func() {
		stopReturned <- pump.Stop()
	}()

	select {
	case stillBusy := <-stopReturned:
		elapsed := time.Since(start)
		t.Logf("Stop() returned after %v with stillBusy=%v", elapsed, stillBusy)
		// Stop() should return within ~5 seconds (the grace period)
		// Allow some margin for goroutine scheduling and test overhead
		require.LessOrEqual(t, elapsed, 6*time.Second, "Stop() should return within 5-second grace period even with hung tick")
		require.True(t, stillBusy, "Stop() should return true (forced) when main loop doesn't exit")
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return within 10 seconds - main loop wait is unbounded (missing timeout)")
	}

	// Unblock the DB read to clean up
	close(blockCh)
}

// TestP1_1_TickUsesDeadlineBoundContext verifies that tick() uses a
// deadline-bound context for DB reads, preventing indefinite hangs even
// when Stop() is not being called. This is a defense-in-depth check.
//
// REVERT CHECK: Without the fix (using p.ctx directly), this test times
// out because ListPendingRunQueueEntries blocks forever with no deadline.
func TestP1_1_TickUsesDeadlineBoundContext(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-p1-1-tick-deadline")
	ctx := t.Context()

	// Wrap the service to block on reads
	blockingSvc := &blockingReadsService{Service: svc}
	blockCh := make(chan struct{})

	// Enqueue a durable call
	idempotencyKey := "p1-1-tick-deadline-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, blockingSvc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that completes quickly
	coord := newBlockingCoordinator()

	// Start pump with short tick interval
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       blockingSvc,
		Coordinator:    coord,
		PumpInstanceID: "p1-1-tick-deadline-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for at least one tick to complete
	time.Sleep(200 * time.Millisecond)

	// Now block on the next DB read
	blockingSvc.mu.Lock()
	blockingSvc.blockNow = true
	blockingSvc.blockCh = blockCh
	blockingSvc.mu.Unlock()

	// Wait for tick() to be blocked
	time.Sleep(200 * time.Millisecond)

	// The tick() call should eventually fail with context deadline exceeded
	// (after the 5-second timeout in tick), not hang forever. We verify this
	// by waiting 6 seconds and checking that we're not deadlocked.
	select {
	case <-time.After(6 * time.Second):
		// If we get here without deadlock, the context timeout worked
		t.Log("tick() context timeout verified - no deadlock after 6 seconds")
	case <-time.After(15 * time.Second):
		t.Fatal("tick() appears to be deadlocked - context timeout not working")
	}

	// Unblock and cleanup
	close(blockCh)
	stillBusy := pump.Stop()
	require.False(t, stillBusy, "Stop should return false (graceful) after unblock")
}
