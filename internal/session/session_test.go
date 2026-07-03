package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/db"
)

func newTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })

	// Run migrations
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
			deleted_todos TEXT NOT NULL DEFAULT '[]'
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
			hidden INTEGER NOT NULL DEFAULT 0
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

		CREATE INDEX idx_files_session_id ON files(session_id);
		CREATE INDEX idx_files_path ON files(path);
		CREATE INDEX idx_messages_session_id ON messages(session_id);
		CREATE INDEX idx_pending_injects_session_id ON pending_injects(session_id);
	`)
	require.NoError(t, err)

	return sqlDB, db.New(sqlDB)
}

// CreateWithID is the primitive behind `crush run --session <id>` idempotent
// CI invocations and behind `app.resolveSession`'s get-or-create branch.
// It must (a) honour the caller-chosen id verbatim, (b) round-trip the title,
// and (c) refuse a second insert with the same id (so the get-or-create flow
// can distinguish "race lost" from a real failure).
func TestCreateWithID(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	t.Run("creates with caller-supplied id", func(t *testing.T) {
		s, err := svc.CreateWithID(ctx, "pr-42", "Review PR 42")
		require.NoError(t, err)
		assert.Equal(t, "pr-42", s.ID)
		assert.Equal(t, "Review PR 42", s.Title)

		got, err := svc.Get(ctx, "pr-42")
		require.NoError(t, err)
		assert.Equal(t, "pr-42", got.ID)
		assert.Equal(t, "Review PR 42", got.Title)
	})

	t.Run("rejects duplicate id", func(t *testing.T) {
		_, err := svc.CreateWithID(ctx, "dup", "first")
		require.NoError(t, err)
		_, err = svc.CreateWithID(ctx, "dup", "second")
		require.Error(t, err, "second insert with the same id must fail (UNIQUE constraint)")
	})

	t.Run("does not collide with uuid-allocated Create", func(t *testing.T) {
		// Create() picks a random UUID; CreateWithID() picks a literal — they
		// must coexist in the same table without trouble.
		uuidSess, err := svc.Create(ctx, "uuid sess")
		require.NoError(t, err)
		idSess, err := svc.CreateWithID(ctx, "named-sess", "named")
		require.NoError(t, err)
		assert.NotEqual(t, uuidSess.ID, idSess.ID)
	})
}

// TestPendingInjects exercises the cross-process inject queue foundation:
// enqueue a row, drain it (which must return it AND delete it), and confirm a
// second drain is empty. It also checks that interrupt rows are surfaced via
// the hasInterrupt flag but neither returned in the merge slice nor deleted.
func TestPendingInjects(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "inject sess")
	require.NoError(t, err)

	t.Run("create, drain returns and deletes, re-drain empty", func(t *testing.T) {
		err := svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID,
			MessageID: "msg-1",
			Content:   "hello from another process",
		})
		require.NoError(t, err)

		merge, hasInterrupt, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.False(t, hasInterrupt)
		require.Len(t, merge, 1)
		assert.Equal(t, "msg-1", merge[0].MessageID)
		assert.Equal(t, "hello from another process", merge[0].Content)
		assert.False(t, merge[0].Interrupt)
		assert.NotEmpty(t, merge[0].ID)

		// Second drain must be empty (delete-after-read).
		merge2, hasInterrupt2, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.False(t, hasInterrupt2)
		assert.Empty(t, merge2)
	})

	t.Run("interrupt rows are reported but not drained", func(t *testing.T) {
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "msg-int", Content: "stop now", Interrupt: true,
		}))
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "msg-merge", Content: "also this",
		}))

		merge, hasInterrupt, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.True(t, hasInterrupt)
		require.Len(t, merge, 1)
		assert.Equal(t, "msg-merge", merge[0].MessageID)

		// The interrupt row must survive; the non-interrupt one is gone.
		merge2, hasInterrupt2, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.True(t, hasInterrupt2, "interrupt row must persist after a non-interrupt drain")
		assert.Empty(t, merge2)
	})
}

// TestConsumeInterruptInject verifies the interrupt-row half of the queue:
// ConsumeInterruptInject must return AND delete the oldest interrupt=true row,
// leave non-interrupt rows untouched, and report (nil, nil) when the queue has
// no interrupt row.
func TestConsumeInterruptInject(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "interrupt sess")
	require.NoError(t, err)

	t.Run("empty queue returns nil", func(t *testing.T) {
		pi, err := svc.ConsumeInterruptInject(ctx, sess.ID)
		require.NoError(t, err)
		assert.Nil(t, pi)
	})

	t.Run("consumes and deletes interrupt row, leaves merge rows", func(t *testing.T) {
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "msg-merge", Content: "merge me",
		}))
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "msg-int", Content: "stop now", Interrupt: true,
		}))

		pi, err := svc.ConsumeInterruptInject(ctx, sess.ID)
		require.NoError(t, err)
		require.NotNil(t, pi)
		assert.Equal(t, "msg-int", pi.MessageID)
		assert.True(t, pi.Interrupt)

		// Second consume is empty (delete-after-read).
		pi2, err := svc.ConsumeInterruptInject(ctx, sess.ID)
		require.NoError(t, err)
		assert.Nil(t, pi2)

		// The non-interrupt row must still be drainable.
		merge, hasInterrupt, err := svc.DrainPendingInjects(ctx, sess.ID)
		require.NoError(t, err)
		assert.False(t, hasInterrupt)
		require.Len(t, merge, 1)
		assert.Equal(t, "msg-merge", merge[0].MessageID)
	})

	t.Run("consumes oldest interrupt row first", func(t *testing.T) {
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "int-old", Interrupt: true, CreatedAt: 1000,
		}))
		require.NoError(t, svc.CreatePendingInject(ctx, PendingInject{
			SessionID: sess.ID, MessageID: "int-new", Interrupt: true, CreatedAt: 2000,
		}))

		pi, err := svc.ConsumeInterruptInject(ctx, sess.ID)
		require.NoError(t, err)
		require.NotNil(t, pi)
		assert.Equal(t, "int-old", pi.MessageID)

		pi2, err := svc.ConsumeInterruptInject(ctx, sess.ID)
		require.NoError(t, err)
		require.NotNil(t, pi2)
		assert.Equal(t, "int-new", pi2.MessageID)
	})
}

func TestUpdateReasoningEffort(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)

	ctx := t.Context()

	// Create a test session
	session, err := svc.Create(ctx, "Test Session")
	require.NoError(t, err)
	require.NotNil(t, session)

	t.Run("sets reasoning effort for both models", func(t *testing.T) {
		err := svc.UpdateReasoningEffort(ctx, session.ID, "high", "low")
		require.NoError(t, err)

		updated, err := svc.Get(ctx, session.ID)
		require.NoError(t, err)
		assert.Equal(t, "high", updated.LargeModelReasoningEffort)
		assert.Equal(t, "low", updated.SmallModelReasoningEffort)
	})

	t.Run("updates only large model effort", func(t *testing.T) {
		err := svc.UpdateReasoningEffort(ctx, session.ID, "max", "")
		require.NoError(t, err)

		updated, err := svc.Get(ctx, session.ID)
		require.NoError(t, err)
		assert.Equal(t, "max", updated.LargeModelReasoningEffort)
		// Empty string overwrites, so small model becomes empty (not preserved)
		assert.Equal(t, "", updated.SmallModelReasoningEffort)
	})

	t.Run("updates only small model effort", func(t *testing.T) {
		// First set both to known values
		err := svc.UpdateReasoningEffort(ctx, session.ID, "high", "high")
		require.NoError(t, err)

		// Then update only small model
		err = svc.UpdateReasoningEffort(ctx, session.ID, "", "medium")
		require.NoError(t, err)

		updated, err := svc.Get(ctx, session.ID)
		require.NoError(t, err)
		// Empty string overwrites large model
		assert.Equal(t, "", updated.LargeModelReasoningEffort)
		assert.Equal(t, "medium", updated.SmallModelReasoningEffort)
	})

	t.Run("supports all valid effort levels", func(t *testing.T) {
		validLevels := []string{"low", "medium", "high", "max"}
		for _, level := range validLevels {
			err := svc.UpdateReasoningEffort(ctx, session.ID, level, level)
			require.NoError(t, err, "level=%s", level)

			updated, err := svc.Get(ctx, session.ID)
			require.NoError(t, err)
			assert.Equal(t, level, updated.LargeModelReasoningEffort)
			assert.Equal(t, level, updated.SmallModelReasoningEffort)
		}
	})

	t.Run("publishes update event", func(t *testing.T) {
		events := svc.Subscribe(ctx)

		// Start goroutine to consume event
		eventCh := make(chan struct{})
		go func() {
			for range events {
				close(eventCh)
				return
			}
		}()

		err := svc.UpdateReasoningEffort(ctx, session.ID, "high", "high")
		require.NoError(t, err)

		select {
		case <-eventCh:
		case <-ctx.Done():
			t.Fatal("timed out waiting for event")
		}
	})
}

func TestCreateSession_DefaultReasoningEffort(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)

	ctx := t.Context()

	session, err := svc.Create(ctx, "Test Session")
	require.NoError(t, err)

	// The DB has DEFAULT 'medium', so when we read back, we get "medium"
	assert.Equal(t, "medium", session.LargeModelReasoningEffort)
	assert.Equal(t, "medium", session.SmallModelReasoningEffort)

	// When we explicitly set a different value, it should override the default
	err = svc.UpdateReasoningEffort(ctx, session.ID, "high", "high")
	require.NoError(t, err)

	retrieved, err := svc.Get(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, "high", retrieved.LargeModelReasoningEffort)
	assert.Equal(t, "high", retrieved.SmallModelReasoningEffort)
}
