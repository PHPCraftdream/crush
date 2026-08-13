// P1-4 regression test (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// RunQueuePump enforces a bounded worker pool to prevent unbounded fan-out
// after process crash with large backlog.
//
// The bug: ListPendingRunQueueEntries returns the entire backlog, and tick()
// dispatches one goroutine per pending entry for each non-busy session. With
// no global limit, a process crash followed by a large backlog could spawn
// hundreds of concurrent provider streams, DB connections, and subprocesses
// in a single tick — indistinguishable from a hang and prone to rate-limit
// storms.
//
// The fix:
// 1. Added RunQueueMaxConcurrentExecutions constant (10 in production).
// 2. Added execSem chan struct{} bounded semaphore to RunQueuePump.
// 3. processEntry attempts non-blocking acquire BEFORE leasing; returns
//    immediately if pool is full (entry stays pending, no lease, no attempt).
// 4. executeEntry releases slot via defer when execution completes.
// 5. Per-session inFlight guard is preserved as an additional constraint.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go, remove the execSem field from RunQueuePump struct.
//  2. Remove the select/default acquire gate in processEntry.
//  3. Remove the defer release in executeEntry.
//  4. Remove all releaseSlot() helper calls in processEntry.
//  5. This test will FAIL with the predicted symptom (more than
//     TestMaxConcurrentExecutions entries execute concurrently).

package session_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// boundedPoolTestCoordinator is a test Coordinator that tracks
// concurrent execution and blocks until explicitly released.
type boundedPoolTestCoordinator struct {
	mu          sync.Mutex
	blockCh     chan struct{} // set before each test, blocks until closed
	entryCount  atomic.Int64
	maxInFlight atomic.Int64 // max concurrent entries observed
	inFlightMu  sync.Mutex
	inFlight    atomic.Int64  // current concurrent entries
	completedCh chan struct{} // closed when all expected entries complete
	closeOnce   sync.Once
}

func newBoundedPoolTestCoordinator() *boundedPoolTestCoordinator {
	return &boundedPoolTestCoordinator{
		completedCh: make(chan struct{}),
	}
}

// Run blocks until blockCh is closed, then returns success.
func (c *boundedPoolTestCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	current := c.inFlight.Add(1)
	c.inFlightMu.Lock()
	if current > c.maxInFlight.Load() {
		c.maxInFlight.Store(current)
	}
	c.inFlightMu.Unlock()

	c.entryCount.Add(1)

	c.mu.Lock()
	ch := c.blockCh
	c.mu.Unlock()

	// Block until blockCh is explicitly closed by the test
	if ch != nil {
		<-ch // This blocks on the unbuffered channel
	}

	c.inFlight.Add(-1)

	var result any = "executed"
	return &result, nil
}

// TestP1_4_BoundedWorkerPoolRespectsLimit verifies that the pump does not
// spawn more executeEntry goroutines than configured, even when there are
// many pending entries across different sessions.
//
// REVERT CHECK: Without the fix, all entries start executing concurrently
// (no semaphore), and maxInFlight exceeds TestMaxConcurrentExecutions.
func TestP1_4_BoundedWorkerPoolRespectsLimit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Create 10 sessions with different IDs to bypass per-session inFlight guard
	const numSessions = 10
	const maxConcurrent = 2 // Small limit to make the test deterministic

	// Setup DB and service
	// Keep :memory: for speed (100ms TTL too slow for file I/O).
	// This test doesn't hit the connection recycling bug that breaks TestP0_3.
	conn, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)

	// Run migrations
	_, err = conn.ExecContext(context.Background(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			summary_message_id TEXT,
			todos TEXT,
			large_model_provider TEXT,
			large_model_id TEXT,
			large_model_reasoning_effort TEXT DEFAULT 'medium',
			small_model_provider TEXT,
			small_model_id TEXT,
			small_model_reasoning_effort TEXT DEFAULT 'medium',
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0,
			deleted_todos TEXT NOT NULL DEFAULT '[]',
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			usage_updated_at INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0,
			cost_checksum INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (parent_session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);

		CREATE TABLE session_run_queue (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			call_data TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			leased_by TEXT,
			leased_at INTEGER,
			lease_expires_at INTEGER,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			terminal_failure INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);

		CREATE INDEX idx_session_run_queue_status ON session_run_queue(status, created_at);
		CREATE INDEX idx_session_run_queue_session_id ON session_run_queue(session_id);
		CREATE INDEX idx_session_run_queue_leased ON session_run_queue(status, lease_expires_at);
	`)
	require.NoError(t, err)

	dbq := db.New(conn)
	svc := session.NewService(dbq, conn)

	// Create sessions and enqueue one entry per session
	for i := 0; i < numSessions; i++ {
		sess, err := svc.Create(ctx, "test-session-p1-4")
		require.NoError(t, err)

		callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
		callDataJSON, err := json.Marshal(callData)
		require.NoError(t, err)
		idempotencyKey := "p1-4-probe-" + sess.ID
		require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))
	}

	// Create a blocking coordinator that will block all workers
	coord := newBoundedPoolTestCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	// Start pump with very small concurrency limit
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:                    svc,
		Coordinator:                 coord,
		PumpInstanceID:              "p1-4-bounded-pool-pump",
		TestTick:                    func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:                100 * time.Millisecond,
		TestMaxConcurrentExecutions: maxConcurrent,
	})
	pump.Start()
	defer pump.Stop()

	// Wait for at least maxConcurrent entries to start executing
	require.Eventually(t, func() bool {
		return coord.entryCount.Load() >= maxConcurrent
	}, 5*time.Second, 20*time.Millisecond,
		"at least maxConcurrent entries should start executing")

	// Give it a moment for any additional workers that might have started
	time.Sleep(500 * time.Millisecond)

	// Debug output
	t.Logf("At verification point: entryCount=%d, maxInFlight=%d",
		coord.entryCount.Load(), coord.maxInFlight.Load())

	// CRITICAL ASSERTION: Verify that max concurrent entries never exceeded
	// the configured limit. This is the core invariant we're testing.
	currentMaxInFlight := coord.maxInFlight.Load()
	require.LessOrEqual(t, currentMaxInFlight, int64(maxConcurrent),
		"max concurrent executions (%d) must not exceed TestMaxConcurrentExecutions (%d)",
		currentMaxInFlight, maxConcurrent)

	// Also verify that NOT all entries have started (since pool is full)
	require.Less(t, coord.entryCount.Load(), int64(numSessions),
		"not all entries should start when pool is full")

	// Unblock all workers and let them complete
	close(blockCh)

	// Verify all entries were eventually processed
	require.Eventually(t, func() bool {
		pending, err := svc.ListPendingRunQueueEntries(ctx)
		if err != nil {
			return false
		}
		return len(pending) == 0
	}, 10*time.Second, 100*time.Millisecond,
		"all pending entries should be processed after unblock")

	// Verify total entries processed equals what we enqueued
	require.Equal(t, int64(numSessions), coord.entryCount.Load(),
		"all enqueued entries should have been processed")
}
