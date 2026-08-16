package cmd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/session"
)

func countAllSessionsCLI(t *testing.T, ctx context.Context, sqlDB *sql.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n))
	return n
}

func countMsgsForSessionCLI(t *testing.T, ctx context.Context, sqlDB *sql.DB, sessionID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n))
	return n
}

// TestForkSessionCLI_HappyPath verifies the CLI fork path (forkSessionCLI,
// invoked by `crush sessions fork`) clones the source session's models,
// system prompt, reasoning effort, and todos, and every message into a new
// session, and honors the CLI-specific --session / --title / --child knobs
// on top of the shared session.Service.ForkSessionTx.
func TestForkSessionCLI_HappyPath(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	src, err := s.Create(ctx, "source")
	require.NoError(t, err)
	require.NoError(t, s.UpdateModels(ctx, src.ID, &session.ModelSlotUpdate{Provider: "provL", Model: "modelL"}, &session.ModelSlotUpdate{Provider: "provS", Model: "modelS"}))
	require.NoError(t, s.UpdateReasoningEffort(ctx, src.ID, "high", "low"))
	require.NoError(t, s.UpdateSystemPrompt(ctx, src.ID, "be careful"))
	require.NoError(t, s.SetTodos(ctx, src.ID, []session.Todo{{Content: "do thing", Status: session.TodoStatusPending}}, nil))

	seedRoles := []string{"user", "assistant", "user"}
	for i, role := range seedRoles {
		_, err := conn.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)`,
			uuid.NewString(), src.ID, role, 100+i, 100+i)
		require.NoError(t, err)
	}

	fork, count, err := forkSessionCLI(ctx, s, src.ID, forkOptions{
		newID:   "custom-fork-id",
		title:   "My Fork",
		asChild: true,
	})
	require.NoError(t, err)
	require.Equal(t, "custom-fork-id", fork.ID)
	require.EqualValues(t, 3, count)

	got, err := s.Get(ctx, fork.ID)
	require.NoError(t, err)
	require.Equal(t, "My Fork", got.Title)
	require.Equal(t, src.ID, got.ParentSessionID)
	require.Equal(t, "provL", got.LargeModelProvider)
	require.Equal(t, "modelS", got.SmallModelID)
	require.Equal(t, "high", got.LargeModelReasoningEffort)
	require.Equal(t, "low", got.SmallModelReasoningEffort)
	require.Equal(t, "be careful", got.SystemPrompt)
	require.Len(t, got.Todos, 1)
	require.Equal(t, "do thing", got.Todos[0].Content)

	require.EqualValues(t, 3, countMsgsForSessionCLI(t, ctx, conn, fork.ID))
}

// TestForkSessionCLI_AtTruncation verifies --at N copies only the first N
// messages (1-indexed).
func TestForkSessionCLI_AtTruncation(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	src, err := s.Create(ctx, "source")
	require.NoError(t, err)
	for i, role := range []string{"user", "assistant", "user", "assistant"} {
		_, err := conn.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)`,
			uuid.NewString(), src.ID, role, 100+i, 100+i)
		require.NoError(t, err)
	}

	fork, count, err := forkSessionCLI(ctx, s, src.ID, forkOptions{atN: 2})
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.EqualValues(t, 2, countMsgsForSessionCLI(t, ctx, conn, fork.ID))
}

// TestForkSessionCLI_AtOutOfRange verifies an out-of-range --at value is
// rejected before any DB row is written.
func TestForkSessionCLI_AtOutOfRange(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	src, err := s.Create(ctx, "source")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, 'user', '[]', 100, 100)`,
		uuid.NewString(), src.ID)
	require.NoError(t, err)

	before := countAllSessionsCLI(t, ctx, conn)

	_, _, err = forkSessionCLI(ctx, s, src.ID, forkOptions{atN: 5})
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of range")

	require.Equal(t, before, countAllSessionsCLI(t, ctx, conn), "no session row should be created on validation failure")
}

// TestForkSessionCLI_EmptySourceNoAt verifies forking a source session with
// zero messages succeeds when --at is left unset (0): the range check must
// be skipped rather than resolving atN to 0 and rejecting it as "out of
// range (1..0)" — a legitimate empty-source fork should not fail with an
// error implying the user passed an invalid --at flag they never set.
func TestForkSessionCLI_EmptySourceNoAt(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	src, err := s.Create(ctx, "source")
	require.NoError(t, err)
	require.Zero(t, countMsgsForSessionCLI(t, ctx, conn, src.ID))

	fork, count, err := forkSessionCLI(ctx, s, src.ID, forkOptions{})
	require.NoError(t, err)
	require.Zero(t, count)

	got, err := s.Get(ctx, fork.ID)
	require.NoError(t, err)
	require.Equal(t, "source fork", got.Title)
}

// TestForkSessionCLI_MidwayFailureRollsBack arms a SQLite trigger that aborts
// the copy of a sentinel-role message placed AFTER two normal messages, then
// asserts the ENTIRE fork (new session row + every copied message) is rolled
// back and the caller receives the error — no partial fork survives. This is
// the CLI-path counterpart to session.TestForkSession_MidwayFailureRollsBack,
// confirming forkSessionCLI's shared ForkSessionTx transaction wrapping is
// equally atomic.
func TestForkSessionCLI_MidwayFailureRollsBack(t *testing.T) {
	t.Parallel()
	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	ctx := context.Background()

	src, err := s.Create(ctx, "source")
	require.NoError(t, err)

	// Two normal messages (copied first), then a sentinel message whose role
	// trips the trigger. created_at orders ListMessagesBySession so the
	// sentinel is reached only after the normal copies succeed.
	for i, role := range []string{"user", "assistant", "TRIPWIRE"} {
		_, err := conn.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)`,
			uuid.NewString(), src.ID, role, 100+i, 100+i)
		require.NoError(t, err)
	}

	// Arm the tripwire AFTER seeding the source so seeding is not aborted.
	// The BEFORE INSERT trigger fires (and aborts) only when forkSessionCLI
	// copies the TRIPWIRE message.
	_, err = conn.ExecContext(ctx, `
		CREATE TRIGGER cli_tripwire_before_insert
		BEFORE INSERT ON messages
		WHEN new.role = 'TRIPWIRE'
		BEGIN
			SELECT RAISE(ABORT, 'injected midway CLI copy failure');
		END`)
	require.NoError(t, err)

	beforeSessions := countAllSessionsCLI(t, ctx, conn)
	beforeSrcMsgs := countMsgsForSessionCLI(t, ctx, conn, src.ID)

	_, _, err = forkSessionCLI(ctx, s, src.ID, forkOptions{newID: "should-not-survive"})
	require.Error(t, err, "midway copy failure must surface as an error to the caller")

	// No new session row leaked from the aborted transaction.
	require.Equal(t, beforeSessions, countAllSessionsCLI(t, ctx, conn),
		"the forked session row must be rolled back along with its messages")

	// The explicit "should-not-survive" ID must not exist at all.
	_, getErr := s.Get(ctx, "should-not-survive")
	require.Error(t, getErr, "the partially-forked session must not be visible")

	// Source session untouched.
	require.Equal(t, beforeSrcMsgs, countMsgsForSessionCLI(t, ctx, conn, src.ID))
}
