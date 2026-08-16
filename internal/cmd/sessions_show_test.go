package cmd

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionsShow_TextOutput(t *testing.T) {
	t.Parallel()

	// Create test database
	conn, q := newTestDB(t)

	// Create a session
	s := session.NewService(q, conn)
	sess, err := s.Create(context.Background(), "test session")
	require.NoError(t, err)
	require.NoError(t, s.UpdateModels(context.Background(), sess.ID, &session.ModelSlotUpdate{Provider: "anthropic", Model: "claude-3-5-sonnet"}, &session.ModelSlotUpdate{Provider: "anthropic", Model: "claude-3-5-haiku"}))

	// Verify session was created
	retrieved, err := s.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "test session", retrieved.Title)
	require.Equal(t, "claude-3-5-sonnet", retrieved.LargeModelID)
}

func TestSessionsShow_WithMessages(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "messages test")
	require.NoError(t, err)

	// Add messages
	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Hello"}},
	})
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "Hi there"}},
	})
	require.NoError(t, err)

	// Verify messages were created
	msgs, err := m.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, message.Assistant, msgs[1].Role)
}

func TestSessionsShow_NotFound(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)

	// Try to get nonexistent session
	_, err := s.Get(context.Background(), "nonexistent-id")
	require.Error(t, err)
}

func newTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	// With modernc's SQLite driver, each pooled connection to ":memory:" is
	// a SEPARATE, independently-empty database — there is no shared backing
	// store across connections the way file-backed SQLite has. Without
	// capping the pool to a single connection, database/sql is free to open
	// a second connection under concurrent use (e.g. a caller holding a
	// transaction open across multiple statements, as sessions_fork_test.go
	// does), and that second connection sees none of the tables created
	// below, failing with "no such table". See the identical fix/comment in
	// internal/session/session_test.go's newTestDB.
	sqlDB.SetMaxOpenConns(1)
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
			deleted_todos TEXT NOT NULL DEFAULT '[]',
			large_model_provider TEXT,
			large_model_id TEXT,
			large_model_reasoning_effort TEXT DEFAULT 'medium',
			small_model_provider TEXT,
			small_model_id TEXT,
			small_model_reasoning_effort TEXT DEFAULT 'medium',
			worker_model_provider TEXT,
			worker_model_id TEXT,
			worker_model_reasoning_effort TEXT,
			reviewer_model_provider TEXT,
			reviewer_model_id TEXT,
			reviewer_model_reasoning_effort TEXT,
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0,
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0
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
			background_job_notice INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			cache_creation_tokens INTEGER,
			cache_read_tokens INTEGER,
			total_tokens INTEGER,
			cost_usd REAL,
			usage_provider TEXT,
			usage_model TEXT,
			cache_support TEXT,
			usage_estimated INTEGER
		);

		CREATE INDEX idx_messages_session_id ON messages(session_id);

		CREATE TABLE pending_injects (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			interrupt INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		);
	`)
	require.NoError(t, err)

	return sqlDB, db.New(sqlDB)
}

// TestReclassifyCrashedAsDone_EndTurn verifies that a session holding a
// stale (dead-PID) lock but whose last assistant message finished cleanly
// (end_turn) is reclassified from "crashed" to "done". This is the
// clean-exit-but-lock-not-yet-swept case that was previously misreported.
func TestReclassifyCrashedAsDone_EndTurn(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "clean exit")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "do it"}},
	})
	require.NoError(t, err)

	// Final assistant message that finished cleanly with end_turn.
	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "done"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	a := &app.App{Messages: m, Sessions: s}
	statusByID := map[string]string{sess.ID: "crashed"}

	got := reclassifyCrashedAsDone(context.Background(), a, []session.Session{sess}, statusByID)
	require.Equal(t, "done", got[sess.ID])
}

// TestReclassifyCrashedAsDone_Canceled confirms that a dead-PID lock whose
// last assistant message did NOT finish cleanly (here: canceled) stays
// "crashed" — the genuine mid-turn-crash / interrupted case.
func TestReclassifyCrashedAsDone_Canceled(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "interrupted")
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonCanceled, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	a := &app.App{Messages: m, Sessions: s}
	statusByID := map[string]string{sess.ID: "crashed"}

	got := reclassifyCrashedAsDone(context.Background(), a, []session.Session{sess}, statusByID)
	require.Equal(t, "crashed", got[sess.ID])
}

// TestReclassifyCrashedAsDone_NoAssistantMessage confirms that a session
// with no assistant message at all stays "crashed" (no clean finish to
// promote on).
func TestReclassifyCrashedAsDone_NoAssistantMessage(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "no assistant")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	statusByID := map[string]string{sess.ID: "crashed"}

	got := reclassifyCrashedAsDone(context.Background(), a, []session.Session{sess}, statusByID)
	require.Equal(t, "crashed", got[sess.ID])
}
