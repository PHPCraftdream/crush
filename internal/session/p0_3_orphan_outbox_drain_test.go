package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestOrphanOutbox_Drainage tests that the pump drains orphan outbox entries
// to the main run queue and marks them as done.
func TestOrphanOutbox_Drainage(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-orphan-drain")

	// Write an entry directly to the orphan outbox (simulating a main queue write failure)
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "orphaned test prompt",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-outbox-test-1"
	err = svc.WriteToOrphanOutbox(t.Context(), outboxID, sess.ID, callDataJSON)
	require.NoError(t, err, "write to orphan outbox should succeed")

	// Verify the entry is pending in the outbox
	pendingOutbox, err := svc.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingOutbox, 1, "should have one pending outbox entry")
	require.Equal(t, outboxID, pendingOutbox[0].ID)

	// Create pump with fast drain tick for testing, but WITHOUT coordinator
	// so it drains but doesn't execute
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    nil, // No coordinator: drain but don't execute
		PumpInstanceID: "test-pump-drain",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 50 * time.Millisecond },
	})

	// Start the pump
	pump.Start()
	time.Sleep(200 * time.Millisecond) // Wait for at least a few drain ticks

	// Stop the pump
	pump.Stop()

	// Verify the outbox entry is gone (marked done and deleted)
	pendingOutbox, err = svc.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Empty(t, pendingOutbox, "outbox entry should be drained and marked done")

	// Verify the entry is in the main run queue
	pendingMain, err := svc.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingMain, 1, "entry should be in main run queue")
	require.Equal(t, sess.ID, pendingMain[0].SessionID)

	// Verify the call data matches
	var mainCallData map[string]any
	err = json.Unmarshal([]byte(pendingMain[0].CallData), &mainCallData)
	require.NoError(t, err)
	require.Equal(t, sess.ID, mainCallData["SessionID"])
	require.Equal(t, "orphaned test prompt", mainCallData["Prompt"])
}

// TestOrphanOutbox_ClaimProtection tests that an already-claimed entry
// is not re-claimed by the same pump instance.
func TestOrphanOutbox_ClaimProtection(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-claim-protection")
	ctx := t.Context()

	// Write an entry to the orphan outbox
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "claim protection test",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-claim-test-1"
	err = svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, callDataJSON)
	require.NoError(t, err)

	// Manually claim the entry (simulating a pump that's already processing it)
	claimed, err := svc.ClaimOrphanOutboxEntry(ctx, outboxID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, "processing", claimed.Status)
	require.Equal(t, int64(1), claimed.Attempts)

	// Create pump and let it tick
	mockCoord := &mockCoordinator{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-claim",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 50 * time.Millisecond },
	})

	pump.Start()
	time.Sleep(200 * time.Millisecond)
	pump.Stop()

	// Verify the entry is still in processing state (not done)
	entry, err := svc.GetOrphanOutboxEntry(ctx, outboxID)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, "processing", entry.Status)

	// Verify only one attempt was made (no double-claim)
	require.Equal(t, int64(1), entry.Attempts)
}

// TestOrphanOutbox_RevertCheck verifies that without drainage,
// orphan outbox entries remain pending forever.
func TestOrphanOutbox_RevertCheck(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-revert-check")
	ctx := t.Context()

	// Write an entry to the orphan outbox
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "revert check test",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-revert-test-1"
	err = svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, callDataJSON)
	require.NoError(t, err)

	// Create pump WITHOUT drainage (TestDrainTick returns a huge duration)
	mockCoord := &mockCoordinator{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-no-drain",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 24 * time.Hour }, // Effectively disabled
	})

	pump.Start()
	time.Sleep(200 * time.Millisecond)
	pump.Stop()

	// Verify the entry is STILL pending (was not drained)
	pendingOutbox, err := svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingOutbox, 1, "outbox entry should still be pending without drainage")
	require.Equal(t, outboxID, pendingOutbox[0].ID)

	// Verify the entry is NOT in the main run queue
	pendingMain, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pendingMain, "entry should NOT be in main run queue without drainage")
}

// transientEnqueueFailSessions wraps a real session.Service and fails the
// first N calls to EnqueueRunQueueEntry, then passes through. Used to prove
// a transient (non-exhausted) drain failure doesn't strand the outbox entry
// in 'processing' forever.
type transientEnqueueFailSessions struct {
	session.Service
	failuresLeft int
}

func (s *transientEnqueueFailSessions) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	if s.failuresLeft > 0 {
		s.failuresLeft--
		return errors.New("transientEnqueueFailSessions: simulated transient enqueue failure")
	}
	return s.Service.EnqueueRunQueueEntry(ctx, idempotencyKey, sessionID, callData)
}

// TestOrphanOutbox_RetryAfterTransientFailure is a regression test: an
// outbox entry whose EnqueueRunQueueEntry attempt fails but hasn't
// exhausted MaxAttempts must be released back to 'pending' so a later
// drain cycle picks it up again — not left stuck in 'processing', which
// ListPendingOrphanOutboxEntries never scans and which would otherwise
// make the entry permanently unreachable (never 'done', never 'failed').
//
// REVERT CHECK: comment out the ReleaseOrphanOutboxEntryForRetry call in
// run_queue_pump.go's processOrphanOutboxEntry (the "not yet exhausted"
// branch) and this test fails — the entry stays in 'processing' forever
// and never reaches the main queue.
func TestOrphanOutbox_RetryAfterTransientFailure(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-orphan-retry")
	ctx := t.Context()

	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "retry after transient failure",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-retry-test-1"
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, callDataJSON))

	// Fail the first drain attempt's enqueue, succeed on the second.
	flaky := &transientEnqueueFailSessions{Service: svc, failuresLeft: 1}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       flaky,
		Coordinator:    nil,
		PumpInstanceID: "test-pump-retry",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 50 * time.Millisecond },
	})

	pump.Start()
	time.Sleep(300 * time.Millisecond) // several drain ticks: first fails, later succeeds
	pump.Stop()

	// The entry must have been released back to pending after the first
	// failure and then successfully drained on a later cycle — not stuck
	// in 'processing'.
	pendingOutbox, err := svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pendingOutbox, "entry should have been drained after the transient failure was retried")

	entry, err := svc.GetOrphanOutboxEntry(ctx, outboxID)
	require.NoError(t, err)
	require.Nil(t, entry, "entry should be gone (marked done/deleted), not stuck in processing")

	pendingMain, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingMain, 1, "entry should have reached the main run queue after retry")
}

// TestOrphanOutbox_ConcurrentExecution tests that the drainage goroutine
// and the main tick goroutine can run concurrently without deadlocks or races.
func TestOrphanOutbox_ConcurrentExecution(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-concurrent-exec")
	ctx := t.Context()

	// Enqueue a normal entry to the main queue
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "normal queue test",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	err = svc.EnqueueRunQueueEntry(ctx, "normal-test-1", sess.ID, callDataJSON)
	require.NoError(t, err)

	// Write an entry to the orphan outbox
	orphanCallData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "orphan outbox test",
	}
	orphanCallDataJSON, err := json.Marshal(orphanCallData)
	require.NoError(t, err)

	outboxID := "orphan-concurrent-test-1"
	err = svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, orphanCallDataJSON)
	require.NoError(t, err)

	// Create pump and run it for a while
	mockCoord := &mockCoordinator{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-concurrent",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 30 * time.Millisecond },
	})

	pump.Start()
	time.Sleep(500 * time.Millisecond) // Run for enough ticks for both paths
	forced := pump.Stop()

	// Verify graceful shutdown (no forced shutdown)
	require.False(t, forced, "pump should shut down gracefully")

	// Verify the normal queue entry was processed
	// (mockCoord.Run returns nil, so the entry should be acked/deleted)
	pendingMain, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pendingMain, "normal queue entry should be processed")

	// Verify the orphan outbox entry was drained
	pendingOutbox, err := svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pendingOutbox, "orphan outbox entry should be drained")
}
