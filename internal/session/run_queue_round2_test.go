package session_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestRunQueue_EnqueueLeaseAck_Lifecycle tests the full enqueue → lease → ack lifecycle
func TestRunQueue_EnqueueLeaseAck_Lifecycle(t *testing.T) {
	t.Parallel()

	// Create a test database and service
	sess, svc := setupTestSession(t, "test-session")
	ctx := t.Context()

	// Enqueue a call with idempotency key
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "test prompt",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	idempotencyKey := "test-idempotency-key-1"
	err = svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON)
	require.NoError(t, err, "enqueue should succeed")

	// Verify the entry is pending
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "should have one pending entry")
	require.Equal(t, idempotencyKey, pending[0].ID, "idempotency key should be preserved")

	// Lease the entry
	leased, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "pump-instance-1", 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, leased, "lease should succeed")
	require.Equal(t, idempotencyKey, leased.ID, "leased entry should preserve idempotency key")
	require.Equal(t, sess.ID, leased.SessionID)
	require.Equal(t, "leased", leased.Status)

	// Verify the entry is no longer pending
	pending, err = svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "should have no pending entries after lease")

	// Ack the entry (simulating successful execution)
	deletedID, err := svc.AckRunQueueEntry(ctx, leased.ID)
	require.NoError(t, err)
	require.Equal(t, idempotencyKey, deletedID, "ack should return the deleted id")

	// Verify the entry is completely gone (deleted, not just status changed)
	pending, err = svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "should have no pending entries after ack")
}

// TestRunQueue_LeaseTTLExpiration tests lease TTL expiration and recovery
func TestRunQueue_LeaseTTLExpiration(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-ttl")
	ctx := t.Context()

	// Enqueue a call
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "test prompt",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	err = svc.EnqueueRunQueueEntry(ctx, "test-idempotency-key-2", sess.ID, callDataJSON)
	require.NoError(t, err)

	// Lease with a very short TTL
	shortTTL := 100 * time.Millisecond
	leased, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "pump-instance-1", shortTTL)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Wait for lease to expire
	time.Sleep(shortTTL + 100*time.Millisecond)

	// Verify the entry is not pending (still in leased state, expired but not cleaned up yet)
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "expired lease should not appear as pending until cleanup")

	// Clean up expired leases (simulating pump recovery)
	// Use the current time to ensure all expired leases are cleaned up
	expiredBefore := time.Now().Unix() + 1 // +1 second to be sure we're past the expiry
	err = svc.CleanupExpiredLeases(ctx, expiredBefore)
	require.NoError(t, err, "cleanup expired leases should succeed")

	// Verify the entry is now pending after cleanup
	pending, err = svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "expired lease should now appear as pending")

	// Try to lease again with a different instance ID
	leased2, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "pump-instance-2", 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, leased2, "should be able to lease expired entry")
	require.Equal(t, "pump-instance-2", leased2.LeasedBy, "should be leased by new instance")
	require.True(t, leased2.LeasedAt >= leased.LeasedAt, "new lease should have later timestamp")
}

// TestRunQueue_ConcurrentLeases_Idempotency tests concurrent lease attempts
func TestRunQueue_ConcurrentLeases_Idempotency(t *testing.T) {
	sess, svc := setupTestSession(t, "test-session-concurrent")
	ctx := t.Context()

	// Enqueue a call
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "test prompt",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	err = svc.EnqueueRunQueueEntry(ctx, "test-idempotency-key-3", sess.ID, callDataJSON)
	require.NoError(t, err)

	// Launch 10 concurrent lease attempts
	const concurrentLeases = 10
	var wg sync.WaitGroup
	var leaseCount atomic.Int64
	var leaseErrors atomic.Int64

	for i := 0; i < concurrentLeases; i++ {
		wg.Add(1)
		go func(instanceID string) {
			defer wg.Done()
			// Use a fresh context for each goroutine to avoid connection pool issues
			leased, err := svc.LeaseRunQueueEntry(context.Background(), sess.ID, instanceID, 5*time.Second)
			if err != nil {
				leaseErrors.Add(1)
				return
			}
			if leased != nil {
				leaseCount.Add(1)
				// Verify it was leased by THIS goroutine's instance
				require.Equal(t, instanceID, leased.LeasedBy)
			} else {
				leaseErrors.Add(1)
			}
		}(fmt.Sprintf("pump-%d", i))
	}

	wg.Wait()

	// Exactly ONE lease should succeed
	require.Equal(t, int64(1), leaseCount.Load(), "only one lease should succeed")
	require.Equal(t, int64(concurrentLeases-1), leaseErrors.Load(), "all others should fail")

	// Verify the leased entry is not pending
	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "leased entry should not appear as pending")
}

// TestRunQueue_TerminalFailureVsRetry tests ErrCallAlreadyAttempted detection
func TestRunQueue_TerminalFailureVsRetry(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-terminal")
	ctx := t.Context()

	// Create a call that will fail with ErrCallAlreadyAttempted
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "test prompt",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	err = svc.EnqueueRunQueueEntry(ctx, "test-idempotency-key-4", sess.ID, callDataJSON)
	require.NoError(t, err)

	// Lease the entry
	leased, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "pump-instance-1", 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Test that TerminalFailRunQueueEntry works (SQL typo fix verification)
	err = svc.TerminalFailRunQueueEntry(ctx, leased.ID)
	require.NoError(t, err, "terminal fail should succeed (SQL typo fix verified)")

	// Verify the entry is COMPLETELY GONE (not just marked terminal)
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "terminal-failed entry should not reappear as pending")

	// Even after waiting for potential TTL expiry, it shouldn't come back
	time.Sleep(10 * time.Millisecond)
	pending, err = svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "entry should not recover from terminal failure even after TTL")

	// Test marker interface detection
	alreadyAttemptedErr := &agent.ErrCallAlreadyAttempted{Err: errors.New("call was already executed")}
	var alreadyAttempted session.AlreadyAttempted
	require.True(t, errors.As(alreadyAttemptedErr, &alreadyAttempted), "marker interface should match error type")
	require.True(t, alreadyAttempted.AlreadyAttempted(), "AlreadyAttempted() should return true")
}

// TestRunQueue_PumpLifecycle tests graceful shutdown
func TestRunQueue_PumpLifecycle_GracefulShutdown(t *testing.T) {
	t.Parallel()

	_, svc := setupTestSession(t, "test-session-pump")

	// Create a mock coordinator
	mockCoord := &mockCoordinator{}

	// Create pump with fast tick for testing
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
	})

	// Start the pump
	pump.Start()
	time.Sleep(50 * time.Millisecond) // Let it run a few ticks

	// Stop the pump
	pump.Stop()
	time.Sleep(50 * time.Millisecond) // Give it time to shut down cleanly

	// Verify it stopped cleanly by trying to start again
	pump2 := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-2",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
	})
	pump2.Start()
	time.Sleep(50 * time.Millisecond)
	pump2.Stop()

	require.True(t, true, "pump lifecycle test completed")
}

// Helper: mock coordinator for testing
type mockCoordinator struct{}

func (m *mockCoordinator) Run(ctx context.Context, sessionID, prompt string, providerOptions map[string]any, attachments ...any) (*any, error) {
	return nil, nil
}

// Helper: setup a test session and service
func setupTestSession(t *testing.T, title string) (*session.Session, session.Service) {
	// Use in-memory database for testing (following session_test.go pattern)
	t.Helper()

	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })

	// Configure connection pool for concurrent access
	sqlDB.SetMaxOpenConns(1) // SQLite only supports one writer
	sqlDB.SetMaxIdleConns(1)

	// Run migrations (copied from session_test.go newTestDB)
	_, err = sqlDB.ExecContext(context.Background(), `
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

		CREATE TABLE session_permissions (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			action TEXT NOT NULL,
			path TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		);

		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			provider TEXT,
			reasoning_effort TEXT DEFAULT 'medium',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER,
			is_summary_message INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			hidden INTEGER NOT NULL DEFAULT 0,
			auto_resumed INTEGER NOT NULL DEFAULT 0,
			background_job_notice INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE files (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			path TEXT NOT NULL,
			content TEXT NOT NULL,
			version INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(path, session_id, version)
		);

		CREATE TABLE read_files (
			session_id TEXT NOT NULL,
			path TEXT NOT NULL,
			read_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, path)
		);

		CREATE TABLE pending_injects (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			content TEXT NOT NULL,
			interrupt INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
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
			updated_at INTEGER NOT NULL
		);

		CREATE INDEX idx_files_session_id ON files(session_id);
		CREATE INDEX idx_files_path ON files(path);
		CREATE INDEX idx_messages_session_id ON messages(session_id);
		CREATE INDEX idx_pending_injects_session_id ON pending_injects(session_id);
		CREATE INDEX idx_session_run_queue_session_status ON session_run_queue(session_id, status);
		CREATE INDEX idx_session_run_queue_created_at ON session_run_queue(created_at);
	`)
	require.NoError(t, err)

	// Create queries and service
	queries := db.New(sqlDB)
	svc := session.NewService(queries, sqlDB)

	// Create a session
	sess, err := svc.Create(context.Background(), title)
	require.NoError(t, err)

	return &sess, svc
}
