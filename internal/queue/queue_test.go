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
