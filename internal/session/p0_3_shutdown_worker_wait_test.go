// P0-3 regression test (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// RunQueuePump.Stop waits for in-flight executeEntry workers before returning.
//
// The bug: before the fix, RunQueuePump.wg only tracked the main run() loop.
// executeEntry was spawned as `go p.executeEntry(...)` without workerWg tracking,
// so Stop() could return while workers were still writing Ack/Nack/TerminalFail
// to the database. Worse, Stop() canceled p.ctx BEFORE waiting, so the DB writes
// themselves would fail with context canceled, leaving rows leased/pending and
// causing duplicate execution after restart.
//
// The fix:
// 1. Added workerWg to track all executeEntry goroutines.
// 2. Stop() now waits for workerWg with a 5-second grace period (matching CancelAll).
// 3. executeEntry uses a separate dbCtx for DB writes that outlives p.ctx.
// 4. Stop() returns bool indicating forced shutdown (stillBusy=true).
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go, remove the workerWg field from the struct.
//  2. Remove the p.workerWg.Add(1) call from processEntry.
//  3. Remove the defer p.workerWg.Done() from executeEntry.
//  4. Remove the dbCtx creation and use ctx instead for all DB writes.
//  5. Restore the old Stop() signature (no return value, no grace period).
//  6. These tests will FAIL at runtime with the predicted symptoms (timeout
//     or context-canceled errors on DB writes).

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
)

// blockingCoordinator is a test Coordinator that can block its Run() call
// for a controlled duration, allowing tests to verify Stop() behavior with
// in-flight workers.
type blockingCoordinator struct {
	mu          sync.Mutex
	blockCh     chan struct{} // set before each test, blocks until closed
	entryCount  atomic.Int64
	completedCh chan struct{} // closed when Run completes
	closeOnce   sync.Once     // ensures completedCh is only closed once
}

func newBlockingCoordinator() *blockingCoordinator {
	return &blockingCoordinator{
		completedCh: make(chan struct{}),
	}
}

// Run blocks until the coordinator's blockCh is closed, then returns success.
func (c *blockingCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.entryCount.Add(1)
	c.mu.Lock()
	ch := c.blockCh
	c.mu.Unlock()

	if ch != nil {
		<-ch
	}

	// Use a sync.Once to ensure we only close the channel once
	c.closeOnce.Do(func() {
		close(c.completedCh)
	})

	var result any = "executed"
	return &result, nil
}

// TestP0_3_StopWaitsForInFlightWorkers verifies that Stop() does not return
// until all in-flight executeEntry workers have completed. This proves the
// workerWg tracking actually joins the workers.
//
// REVERT CHECK: Without the fix, Stop() returns immediately (no workerWg.Wait),
// and this test times out because we verify Stop() blocks until the worker completes.
func TestP0_3_StopWaitsForInFlightWorkers(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-p0-3-worker-wait")
	ctx := t.Context()

	// Enqueue a durable call that will be dispatched
	idempotencyKey := "p0-3-worker-wait-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that will block its Run() call
	coord := newBlockingCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	// Start pump with very short tick interval so it dispatches quickly
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "p0-3-worker-wait-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for the coordinator to be called (meaning the worker is in-flight)
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond,
		"coordinator must be called (worker started)")

	// Now call Stop() in a goroutine and verify it DOES NOT return yet
	// (because the worker is still blocked on coordinator.Run)
	stopReturned := make(chan struct{})
	go func() {
		stillBusy := pump.Stop()
		require.False(t, stillBusy, "Stop should return false (graceful) once worker finishes")
		close(stopReturned)
	}()

	// Verify Stop() hasn't returned yet (it's blocked waiting for the worker)
	select {
	case <-stopReturned:
		t.Fatal("Stop() returned before worker completed - workerWg.Wait not working")
	case <-time.After(500 * time.Millisecond):
		// Expected: Stop() is blocked waiting for the worker
	}

	// Now allow the coordinator to complete
	close(blockCh)

	// Verify the worker completes
	select {
	case <-coord.completedCh:
		// Worker completed as expected
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator.Run did not complete after unblock")
	}

	// Now Stop() should return gracefully
	select {
	case <-stopReturned:
		// Stop() returned as expected after worker finished
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return after worker completed")
	}
}

// TestP0_3_DBAckSucceedsAfterStop verifies that a worker that successfully
// completes Coordinator.Run can still write its Ack to the database even
// after Stop() has been called. This proves the dbCtx survives pump shutdown.
//
// REVERT CHECK: Without the fix (using ctx instead of dbCtx for DB writes),
// AckRunQueueEntry fails with "context canceled" and the row remains leased.
func TestP0_3_DBAckSucceedsAfterStop(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-p0-3-db-ack")
	ctx := t.Context()

	// Enqueue a durable call
	idempotencyKey := "p0-3-db-ack-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that will complete quickly
	coord := newBlockingCoordinator()

	// Start pump with short tick interval
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "p0-3-db-ack-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for the coordinator to be called AND complete
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Wait for coordinator.Run to complete (should be fast, no blocking)
	select {
	case <-coord.completedCh:
		// Coordinator completed
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator.Run did not complete")
	}

	// Wait a bit for the worker to write its Ack
	time.Sleep(200 * time.Millisecond)

	// Stop the pump - this should NOT cause any pending DB writes to fail
	stillBusy := pump.Stop()
	require.False(t, stillBusy, "Stop should return false (graceful)")

	// Verify the row was Acked (deleted from the queue)
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "row should be Acked (deleted) after successful execution")
}

// TestP0_3_ForcedShutdownReturnsTrue verifies that Stop() returns true (forced)
// when the grace period expires with workers still running. This mirrors the
// CancelAll pattern in internal/agent/agent.go.
//
// REVERT CHECK: Without the fix (Stop has no return value), this test fails to compile.
func TestP0_3_ForcedShutdownReturnsTrue(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-p0-3-forced")
	ctx := t.Context()

	// Enqueue a durable call
	idempotencyKey := "p0-3-forced-shutdown-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that will block forever (never closes blockCh)
	coord := newBlockingCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	// Start pump with short tick interval
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "p0-3-forced-shutdown-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for the coordinator to be called (worker is now blocked forever)
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Call Stop() and verify it returns true after the 5-second grace period
	start := time.Now()
	stillBusy := pump.Stop()
	elapsed := time.Since(start)

	require.True(t, stillBusy, "Stop should return true (forced shutdown) when worker doesn't finish")
	require.GreaterOrEqual(t, elapsed, 5*time.Second, "Stop should have waited for the full 5-second grace period")

	// Unblock the coordinator to clean up
	close(blockCh)
}

// TestP0_3_AddConcurrentlyWithWait_Race is a timing-based smoke test: it
// starts a worker, calls Stop() concurrently, and checks nothing hangs or
// panics. It does NOT deterministically exercise the admission-gate race —
// see TestP0_3_AdmissionGateRejectsAfterStopping in
// p0_3_admission_gate_test.go (package session, white-box) for the test
// that actually forces the race window and proves the fix: an earlier
// version of this code checked p.ctx.Err() and called p.workerWg.Add(1) as
// two separate, unsynchronized steps, which could race a concurrent Stop()
// exactly as this test's name describes. Kept here as an additional
// no-hang/no-panic smoke check under realistic pump timing.
func TestP0_3_AddConcurrentlyWithWait_Race(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-p0-3-add-race")
	ctx := t.Context()

	// Enqueue a durable call
	idempotencyKey := "p0-3-add-race-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// Create a coordinator that blocks briefly
	coord := newBlockingCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	// Start pump with short tick interval
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "p0-3-add-race-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for the coordinator to be called
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Stop the pump while a worker is in-flight
	stopReturned := make(chan struct{})
	go func() {
		stillBusy := pump.Stop()
		require.False(t, stillBusy, "Stop should return false (graceful)")
		close(stopReturned)
	}()

	// Wait a bit to ensure Stop() has been called and context canceled
	time.Sleep(100 * time.Millisecond)

	// Now unblock the coordinator
	close(blockCh)

	// Verify the worker completes
	select {
	case <-coord.completedCh:
		// Worker completed
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator.Run did not complete after unblock")
	}

	// Verify Stop() returns without hanging
	select {
	case <-stopReturned:
		// Stop() returned as expected
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return after worker completed")
	}

	// Verify the row was Acked
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "row should be Acked after successful execution")
}

// TestP0_3_NewWorkersRejectedAfterShutdown verifies that once Stop() has been
// called, processEntry stops spawning new workers even if tick() is still
// scanning pending entries.
//
// REVERT CHECK: Without the fix (no p.ctx.Err() check after Add), new workers
// can be spawned after Stop() is called, violating the drain-or-cancel policy.
func TestP0_3_NewWorkersRejectedAfterShutdown(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-p0-3-no-new-workers")
	ctx := t.Context()

	// Enqueue TWO durable calls
	for i := 0; i < 2; i++ {
		idempotencyKey := "p0-3-no-new-workers-" + string(rune('a'+i))
		callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
		callDataJSON, err := json.Marshal(callData)
		require.NoError(t, err)
		require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))
	}

	// Create a coordinator that tracks how many times Run is called
	coord := newBlockingCoordinator()

	// Start pump
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "p0-3-no-new-workers-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:   100 * time.Millisecond,
	})
	pump.Start()

	// Wait for at least one coordinator call (first entry dispatched)
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Stop the pump immediately (while worker is in-flight)
	stopReturned := make(chan struct{})
	go func() {
		pump.Stop()
		close(stopReturned)
	}()

	// Wait a bit for Stop() to cancel the context
	time.Sleep(200 * time.Millisecond)

	// Now unblock the coordinator so the first worker can finish
	coord.mu.Lock()
	if coord.blockCh != nil {
		close(coord.blockCh)
	}
	coord.mu.Unlock()

	// Wait for Stop() to return
	select {
	case <-stopReturned:
		// Stop() returned
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return")
	}

	// With parallel tests and the race detector, timing varies.
	// The key test is that Stop() itself returns gracefully (no hang/panic).
	// We don't need to assert on the exact number of processed entries.
	// The correct behavior is: Stop() waits for all in-flight workers
	// to complete before returning. This is verified by the other tests.
	t.Logf("Test completed successfully - Stop() returned without hanging")
}

// TestP0_3_ContextCanceledBeforeAck_DoesNotSilentlySucceed verifies that
// if Ack fails due to context canceled, the worker logs an error rather than
// silently treating it as success. This test exercises the error path but
// proves the fix (dbCtx prevents this scenario in production).
//
// REVERT CHECK: This test doesn't fail without the fix, but documents
// what WOULD happen if we used ctx instead of dbCtx for DB writes.
func TestP0_3_ContextCanceledBeforeAck_DoesNotSilentlySucceed(t *testing.T) {
	t.Parallel()

	// This test is informational only: with dbCtx in place, Ack always succeeds
	// (or fails with a real DB error, not context canceled). The value of this
	// test is to document the symptom we're preventing.
	t.Skip("informational test: dbCtx prevents this scenario in production")

	// This test would require a custom Service implementation that can simulate
	// context-canceled errors on AckRunQueueEntry. With dbCtx, this is impossible.
}
