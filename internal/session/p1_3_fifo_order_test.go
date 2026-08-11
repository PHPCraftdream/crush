package session_test

// P1-3 regression test (found during 2026-08-11 release readiness review):
// GetOldestPendingRunQueueEntryForSession and ListPendingRunQueueEntries
// both used ORDER BY created_at ASC without any tie-breaker for entries
// enqueued within the same second. Since created_at is populated via
// time.Now().Unix() (second precision), multiple entries enqueued in
// quick succession (parallel agents, interrupts, or just fast loops) get
// the same created_at value. SQLite does not guarantee stable order among
// rows with equal sort keys, so FIFO order can be violated.
//
// Fixed by adding rowid ASC as a secondary sort key to both queries:
// - GetOldestPendingRunQueueEntryForSession (per-session FIFO)
// - ListPendingRunQueueEntries (pump scanning order)
//
// The session_run_queue table uses id TEXT PRIMARY KEY (not an INTEGER
// PRIMARY KEY rowid alias) and is not declared WITHOUT ROWID, so SQLite
// maintains an implicit rowid column that monotonically increases on
// INSERT (guaranteed FIFO order of insertion). This provides a deterministic
// tie-breaker without requiring any schema migration.
//
// REVERT CHECK PROCEDURE:
//  1. In internal/db/sql/run_queue.sql, remove ", rowid ASC" from both
//     GetOldestPendingRunQueueEntryForSession and ListPendingRunQueueEntries.
//  2. Run: go test -run TestReleaseGate_P1_3_FIFOOrderPreservedWithSameCreatedAt -v -race -count=20
//  3. EMPIRICAL FINDING (2026-08-11): In our test environment, SQLite's
//     default b-tree scan order for fresh inserts happens to match rowid
//     order, so the test still passes even without the explicit tie-breaker.
//     THIS IS NOT A GUARANTEE. The SQL standard does NOT define behavior
//     for ORDER BY with equal sort keys - it is implementation-defined.
//     Future SQLite versions, different query plans (e.g. index usage),
//     or vacuum/analyze operations could all change the order.
//  4. With ", rowid ASC" restored, ALWAYS PASS: order is guaranteed by
//     SQLite's explicit rowid tie-breaker, not by accident.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReleaseGate_P1_3_FIFOOrderPreservedWithSameCreatedAt proves that
// multiple entries enqueued for the same session within the same second
// are leased in strict FIFO order (first enqueued = first leased), even
// when all have identical created_at values.
//
// The test forces identical created_at values by enqueuing in a tight
// loop without any delay (the entire loop typically completes in well
// under one second, so all entries get the same time.Now().Unix() value).
// Each entry's enqueue order is recorded in its callData for later
// verification.
//
// Without the rowid tie-breaker, this test would occasionally fail
// in theory (flaky), but in practice SQLite's b-tree storage happens to
// preserve insert order for fresh rows in our test environment. This is an
// implementation detail, not a guarantee - the fix is still necessary for
// production correctness. With rowid ASC, the order is deterministic and
// the test always passes.
func TestReleaseGate_P1_3_FIFOOrderPreservedWithSameCreatedAt(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-session-fifo-same-created")
	ctx := t.Context()

	// Enqueue multiple entries in rapid succession — this typically
	// completes within a single Unix second, giving all entries the
	// same created_at value.
	const numEntries = 5
	type orderedCallData struct {
		SessionID    string
		Prompt       string
		EnqueueOrder int // Recorded to verify FIFO order
	}

	enqueueOrder := make([]string, 0, numEntries)

	for i := 0; i < numEntries; i++ {
		callData := orderedCallData{
			SessionID:    sess.ID,
			Prompt:       "test prompt",
			EnqueueOrder: i,
		}
		callDataJSON, err := json.Marshal(callData)
		require.NoError(t, err)

		idempotencyKey := "fifo-probe-" + sess.ID + "-" + string(rune('0'+i))
		require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

		// Record the idempotency key (which contains the order index)
		enqueueOrder = append(enqueueOrder, idempotencyKey)
	}

	// Now lease entries one at a time and verify they come out in the
	// same order they were enqueued.
	leaseOrder := make([]string, 0, numEntries)
	for i := 0; i < numEntries; i++ {
		leased, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "fifo-test-pump", 10*time.Second)
		require.NoError(t, err)
		require.NotNil(t, leased, "must be able to lease all %d entries in order", numEntries)

		// Parse the callData to extract the enqueue order
		var parsed orderedCallData
		require.NoError(t, json.Unmarshal([]byte(leased.CallData), &parsed))

		// Verify that the leased entry is exactly the i-th enqueued one
		require.Equal(t, i, parsed.EnqueueOrder,
			"entry %d must be leased in the same order it was enqueued (FIFO)", i+1)

		leaseOrder = append(leaseOrder, leased.ID)

		// Ack the entry so the next one can be leased
		deletedID, err := svc.AckRunQueueEntry(ctx, leased.ID, "fifo-test-pump")
		require.NoError(t, err)
		require.Equal(t, leased.ID, deletedID, "must delete exactly the leased entry")
	}

	// Verify that the lease order exactly matches the enqueue order
	require.Equal(t, enqueueOrder, leaseOrder,
		"all entries must be leased in strict FIFO order")

	// Verify that the queue is now empty
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "queue must be empty after all entries are processed")
}
