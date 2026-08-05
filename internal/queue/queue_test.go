package queue

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	_, err = db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS queue_tasks (
		id TEXT PRIMARY KEY,
		session_id TEXT,
		prompt TEXT NOT NULL,
		role TEXT,
		max_cost REAL,
		max_tokens INTEGER,
		timeout_sec INTEGER,
		status TEXT NOT NULL CHECK(status IN ('pending','running','done','failed','cancelled')),
		cost REAL DEFAULT 0,
		tokens INTEGER DEFAULT 0,
		exit_reason TEXT,
		created_at INTEGER NOT NULL,
		started_at INTEGER,
		finished_at INTEGER
	)`)
	require.NoError(t, err)
	return db
}

func TestQueue_AddAndList(t *testing.T) {
	db := setupTestDB(t)
	q := NewService(db)
	ctx := context.Background()

	id, err := q.Add(ctx, "", "hello", "fast", 0, 0, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	tasks, err := q.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "hello", tasks[0].Prompt)
	assert.Equal(t, "fast", tasks[0].Role)
	assert.Equal(t, StatusPending, tasks[0].Status)
}

func TestQueue_ClaimPending(t *testing.T) {
	db := setupTestDB(t)
	q := NewService(db)
	ctx := context.Background()

	_, _ = q.Add(ctx, "", "a", "", 0, 0, 0)
	_, _ = q.Add(ctx, "", "b", "", 0, 0, 0)
	_, _ = q.Add(ctx, "", "c", "", 0, 0, 0)

	claimed, err := q.ClaimPending(ctx, 2)
	require.NoError(t, err)
	require.Len(t, claimed, 2)

	remaining, err := q.List(ctx, StatusPending)
	require.NoError(t, err)
	assert.Len(t, remaining, 1)

	running, err := q.List(ctx, StatusRunning)
	require.NoError(t, err)
	assert.Len(t, running, 2)
}

func TestQueue_UpdateStatusAndGet(t *testing.T) {
	db := setupTestDB(t)
	q := NewService(db)
	ctx := context.Background()

	id, _ := q.Add(ctx, "", "test", "", 0, 0, 0)

	err := q.UpdateStatus(ctx, id, StatusDone, 0.05, 1234, "stop")
	require.NoError(t, err)

	task, err := q.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, StatusDone, task.Status)
	assert.InDelta(t, 0.05, task.Cost, 0.001)
	assert.Equal(t, int64(1234), task.Tokens)
	assert.Equal(t, "stop", task.ExitReason)
}

func TestQueue_RemoveAndClear(t *testing.T) {
	db := setupTestDB(t)
	q := NewService(db)
	ctx := context.Background()

	id1, _ := q.Add(ctx, "", "a", "", 0, 0, 0)
	id2, _ := q.Add(ctx, "", "b", "", 0, 0, 0)

	require.NoError(t, q.Remove(ctx, id1))

	tasks, _ := q.List(ctx, "")
	assert.Len(t, tasks, 1)
	assert.Equal(t, id2, tasks[0].ID)

	require.NoError(t, q.Clear(ctx, StatusPending))
	tasks, _ = q.List(ctx, "")
	assert.Len(t, tasks, 0)
}

func TestQueue_ClaimPending_Empty(t *testing.T) {
	db := setupTestDB(t)
	q := NewService(db)
	ctx := context.Background()

	claimed, err := q.ClaimPending(ctx, 5)
	require.NoError(t, err)
	assert.Len(t, claimed, 0)
}

// TestQueue_ReclaimRunning_ResetsOrphanedTaskToPending is the regression test
// for finding 3 of task #272 (P1-3): a task claimed into 'running' by a
// runner that then crashed/was killed before writing a final status used to
// stay 'running' forever — no future `queue run` would ever pick it up again
// (ClaimPending only looks at 'pending' rows). ReclaimRunning is the
// recovery path queueRunCmd now calls, once per invocation, immediately
// after winning the process-exclusive queue.lock.
func TestQueue_ReclaimRunning_ResetsOrphanedTaskToPending(t *testing.T) {
	db := setupTestDB(t)
	q := NewService(db)
	ctx := context.Background()

	id, err := q.Add(ctx, "", "orphaned", "", 0, 0, 0)
	require.NoError(t, err)

	claimed, err := q.ClaimPending(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// Simulate the abandoned runner having partially written metrics before
	// it died (e.g. a crash between running the child and calling
	// UpdateStatus) — ReclaimRunning must wipe these so the task starts
	// completely clean on its next attempt, not with a previous attempt's
	// stale cost/tokens/exit_reason bleeding into the retry.
	_, err = db.ExecContext(ctx, "UPDATE queue_tasks SET cost = 1.23, tokens = 99, exit_reason = 'leftover' WHERE id = ?", id)
	require.NoError(t, err)

	task, err := q.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, task.Status)
	require.True(t, task.StartedAt.Valid, "precondition: started_at must be set by ClaimPending")

	reclaimed, err := q.ReclaimRunning(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, reclaimed)

	task, err = q.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, task.Status, "orphaned running task must become pending again, or it is unreachable forever")
	assert.Equal(t, float64(0), task.Cost)
	assert.Equal(t, int64(0), task.Tokens)
	assert.Equal(t, "", task.ExitReason)
	assert.False(t, task.StartedAt.Valid, "started_at must be cleared so the task looks like a fresh, never-attempted task")
	assert.False(t, task.FinishedAt.Valid)

	// The reclaimed task must be claimable again by a subsequent queue run —
	// this is the actual symptom the bug produced (task permanently
	// unreachable by any future `queue run`).
	reclaimedTasks, err := q.ClaimPending(ctx, 1)
	require.NoError(t, err)
	require.Len(t, reclaimedTasks, 1)
	assert.Equal(t, id, reclaimedTasks[0].ID)
}

// TestQueue_ReclaimRunning_DoesNotTouchPendingDoneOrFailed guards against a
// reclaim implementation that is too broad (e.g. a WHERE clause that matches
// more than 'running'). Only orphaned 'running' rows may be reset; pending,
// done, and failed tasks must be left exactly as they are.
func TestQueue_ReclaimRunning_DoesNotTouchPendingDoneOrFailed(t *testing.T) {
	db := setupTestDB(t)
	q := NewService(db)
	ctx := context.Background()

	pendingID, err := q.Add(ctx, "", "still-pending", "", 0, 0, 0)
	require.NoError(t, err)

	doneID, err := q.Add(ctx, "", "already-done", "", 0, 0, 0)
	require.NoError(t, err)
	require.NoError(t, q.UpdateStatus(ctx, doneID, StatusDone, 0.5, 100, "stop"))

	failedID, err := q.Add(ctx, "", "already-failed", "", 0, 0, 0)
	require.NoError(t, err)
	require.NoError(t, q.UpdateStatus(ctx, failedID, StatusFailed, 0, 0, "error"))

	reclaimed, err := q.ReclaimRunning(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, reclaimed, "no 'running' rows exist, so reclaim must be a no-op")

	pending, err := q.Get(ctx, pendingID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, pending.Status)

	done, err := q.Get(ctx, doneID)
	require.NoError(t, err)
	assert.Equal(t, StatusDone, done.Status)
	assert.InDelta(t, 0.5, done.Cost, 0.001, "done task's recorded cost must survive an unrelated reclaim call")

	failed, err := q.Get(ctx, failedID)
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, failed.Status)
}
