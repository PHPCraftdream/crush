package session

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/google/uuid"
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
			deleted_todos TEXT NOT NULL DEFAULT '[]',
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0
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

// TestGetCallTreeActivity_NoMessages confirms a freshly created session with
// no messages at all reports ok=false (nothing to report yet), not an error.
func TestGetCallTreeActivity_NoMessages(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "empty")
	require.NoError(t, err)

	act, ok, err := svc.GetCallTreeActivity(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, ok)
	require.Zero(t, act.LatestUnix)
}

// TestGetCallTreeActivityBatch_Empty confirms calling the batch method with
// zero root IDs returns an empty, non-nil map without touching the DB.
func TestGetCallTreeActivityBatch_Empty(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	got, err := svc.GetCallTreeActivityBatch(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

// TestGetCallTreeActivityBatch_Chunking exercises the service-layer chunking
// of GetCallTreeActivityBatch: a root list larger than
// callTreeActivityBatchChunkSize must be split across multiple underlying
// queries and EVERY root's activity must come back in the merged map, not
// just the first chunk's. This guards against a regression where the
// chunking loop breaks early or drops trailing roots (e.g. an off-by-one on
// the final partial chunk). roots = chunkSize*3 + 7 exercises three full
// chunks plus a partial fourth.
func TestGetCallTreeActivityBatch_Chunking(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const roots = callTreeActivityBatchChunkSize*3 + 7

	tx, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	rootIDs := make([]string, 0, roots)
	for i := 0; i < roots; i++ {
		sid := uuid.NewString()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
			 VALUES (?, NULL, 'root', 100, 100)`, sid)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
			 VALUES (?, ?, 'assistant', '[]', 200, 200)`, uuid.NewString(), sid)
		require.NoError(t, err)
		rootIDs = append(rootIDs, sid)
	}
	require.NoError(t, tx.Commit())

	got, err := svc.GetCallTreeActivityBatch(ctx, rootIDs)
	require.NoError(t, err)
	require.Len(t, got, roots, "every root across all chunks must be present in the merged result")
}

// TestConcurrentRenameAndUsage_NoDataLoss is the regression guard for the
// "broad Save clobbers concurrent edits" data-loss bug (#128). A rename
// (title column) running concurrently with a usage update (token columns)
// must leave BOTH changes visible, because the narrow Rename / SetUsage
// methods touch disjoint columns. Under the old single Save — which
// re-wrote every column from a stale snapshot — the loser's change was
// silently overwritten (last-writer-wins).
func TestConcurrentRenameAndUsage_NoDataLoss(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	// Model production exactly: the real pool is single-connection
	// (db/connect.go SetMaxOpenConns(1)), so SQL ops serialize and the race
	// is Go-level interleaving across goroutines sharing that one conn. A
	// plain ":memory:" is also per-connection, so pinning the pool to 1
	// keeps every goroutine on the same in-memory schema.
	sqlDB.SetMaxOpenConns(1)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const iterations = 200
	for i := 0; i < iterations; i++ {
		sess, err := svc.Create(ctx, "init")
		require.NoError(t, err)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, svc.Rename(ctx, sess.ID, "renamed-title"))
		}()
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, svc.SetUsage(ctx, sess.ID, 111, 222))
		}()
		close(start)
		wg.Wait()

		got, err := svc.Get(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, "renamed-title", got.Title, "iter %d: rename lost — title clobbered by concurrent usage write", i)
		assert.Equal(t, int64(111), got.PromptTokens, "iter %d: usage lost — tokens clobbered by concurrent rename", i)
		assert.Equal(t, int64(222), got.CompletionTokens, "iter %d: completion tokens lost", i)
	}
}

// TestConcurrentRenameAndTodos_NoDataLoss extends the guard to the todos
// column: a rename (title) and a todos edit racing must both survive.
func TestConcurrentRenameAndTodos_NoDataLoss(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	sqlDB.SetMaxOpenConns(1) // production-faithful single-connection pool; see TestConcurrentRenameAndUsage_NoDataLoss.
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const iterations = 200
	newTodos := []Todo{{Content: "ship it", Status: TodoStatusInProgress, ActiveForm: "shipping"}}
	for i := 0; i < iterations; i++ {
		sess, err := svc.Create(ctx, "init")
		require.NoError(t, err)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, svc.Rename(ctx, sess.ID, "renamed"))
		}()
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, svc.SetTodos(ctx, sess.ID, newTodos, nil))
		}()
		close(start)
		wg.Wait()

		got, err := svc.Get(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, "renamed", got.Title, "iter %d: title lost", i)
		require.Len(t, got.Todos, 1, "iter %d: todos lost", i)
		assert.Equal(t, "ship it", got.Todos[0].Content)
	}
}

// TestBroadOverwriteLosesConcurrentUsage deterministically reproduces the
// EXACT mechanism the narrow methods fix. It simulates the deleted Save /
// old handleRenameSession behaviour — read a full snapshot, mutate only the
// title, then write ALL columns back from that now-stale snapshot — and
// forces the losing interleaving with channels so it is not flaky. The
// concurrent SetUsage lands between the read and the write, so the stale
// snapshot's prompt_tokens=0 clobbers the 111. This documents WHY Save was
// removed: any caller that re-writes unrelated columns from a snapshot can
// lose a concurrent writer's update to those columns.
func TestBroadOverwriteLosesConcurrentUsage(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	sqlDB.SetMaxOpenConns(1) // production-faithful single-connection pool; see TestConcurrentRenameAndUsage_NoDataLoss.
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "init")
	require.NoError(t, err)

	// Reset to a known state, then force the losing interleaving.
	require.NoError(t, svc.SetUsage(ctx, sess.ID, 0, 0))
	require.NoError(t, svc.Rename(ctx, sess.ID, "init"))

	readDone := make(chan struct{})
	usageDone := make(chan struct{})
	var (
		wg       sync.WaitGroup
		broadErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 1. Read a full snapshot (prompt_tokens still 0 here).
		var (
			sbTitle, sbSummary, sbTodos, sbDeleted string
			sbPrompt, sbCompletion                 int64
		)
		rerr := sqlDB.QueryRowContext(ctx,
			`SELECT title, prompt_tokens, completion_tokens,
			        COALESCE(summary_message_id, ''), COALESCE(todos, ''), deleted_todos
			 FROM sessions WHERE id = ?`,
			sess.ID,
		).Scan(&sbTitle, &sbPrompt, &sbCompletion, &sbSummary, &sbTodos, &sbDeleted)
		if rerr != nil {
			broadErr = rerr
			close(readDone)
			return
		}
		close(readDone) // snapshot captured with prompt_tokens=0
		<-usageDone     // wait for the concurrent usage update to land (now 111)
		// 3. Write ALL columns back from the stale snapshot → clobbers tokens.
		_, broadErr = sqlDB.ExecContext(ctx,
			`UPDATE sessions SET title=?, prompt_tokens=?, completion_tokens=?,
			        summary_message_id=?, todos=?, deleted_todos=?, updated_at=strftime('%s','now')
			 WHERE id=?`,
			"renamed", sbPrompt, sbCompletion, sbSummary, sbTodos, sbDeleted, sess.ID,
		)
	}()

	<-readDone
	// 2. Usage update lands AFTER the snapshot read, BEFORE the write.
	require.NoError(t, svc.SetUsage(ctx, sess.ID, 111, 222))
	close(usageDone)
	wg.Wait()
	require.NoError(t, broadErr)

	got, err := svc.Get(ctx, sess.ID)
	require.NoError(t, err)
	// The broad-overwrite renamed the title but stomped prompt_tokens back
	// to the stale 0 — exactly the data loss the narrow methods prevent.
	assert.Equal(t, "renamed", got.Title, "title should have been renamed")
	assert.Equal(t, int64(0), got.PromptTokens,
		"broad read-modify-write overwriting all columns MUST have clobbered the concurrent usage update — if this is 111 the test is no longer reproducing the old bug")
}

// TestTitleGenerationDoesNotRaceMainTurnTokens is the regression guard for
// P1.6: the title-generation goroutine (agent.go's generateTitle) used to
// call the now-removed UpdateTitleAndUsage, which additively bumped
// prompt_tokens/completion_tokens on top of whatever the main turn's
// SetUsage snapshot last wrote. Since title generation runs concurrently
// with the main turn (see agent.go Run's wg.Go around the generateTitle
// call), the final token counters depended on which of the two finished
// last — nondeterministic. The fix: title generation now calls Rename
// (title only) + IncrementCost (cost only), leaving prompt_tokens/
// completion_tokens exclusively owned by SetUsage's overwrite semantics.
//
// This test drives both orderings explicitly (title-finishes-first and
// title-finishes-last) and asserts that in BOTH cases:
//   - prompt_tokens/completion_tokens end up exactly what SetUsage wrote
//     (title generation must never contribute to them), and
//   - cost ends up as the SUM of the main turn's cost and the title
//     generation's cost (both contributions must land, regardless of order).
func TestTitleGenerationDoesNotRaceMainTurnTokens(t *testing.T) {
	const (
		mainPromptTokens     = int64(1000)
		mainCompletionTokens = int64(200)
		mainCost             = 0.05
		titleCost            = 0.001
	)

	runOrdering := func(t *testing.T, titleFirst bool) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB)
		ctx := t.Context()

		sess, err := svc.Create(ctx, "Untitled Session")
		require.NoError(t, err)
		sessionID := sess.ID

		mainTurn := func() {
			require.NoError(t, svc.SetUsage(ctx, sessionID, mainPromptTokens, mainCompletionTokens))
			_, err := svc.IncrementCost(ctx, sessionID, mainCost)
			require.NoError(t, err)
		}
		titleGen := func() {
			require.NoError(t, svc.Rename(ctx, sessionID, "Generated Title"))
			_, err := svc.IncrementCost(ctx, sessionID, titleCost)
			require.NoError(t, err)
		}

		if titleFirst {
			titleGen()
			mainTurn()
		} else {
			mainTurn()
			titleGen()
		}

		final, err := svc.Get(ctx, sessionID)
		require.NoError(t, err)

		assert.Equal(t, mainPromptTokens, final.PromptTokens,
			"prompt_tokens must equal exactly what SetUsage wrote, unaffected by title generation")
		assert.Equal(t, mainCompletionTokens, final.CompletionTokens,
			"completion_tokens must equal exactly what SetUsage wrote, unaffected by title generation")
		assert.InDelta(t, mainCost+titleCost, final.Cost, 1e-9,
			"cost must include both the main turn's and title generation's contributions")
		assert.Equal(t, "Generated Title", final.Title)
	}

	t.Run("title finishes first", func(t *testing.T) {
		runOrdering(t, true)
	})
	t.Run("title finishes last", func(t *testing.T) {
		runOrdering(t, false)
	})
}

// TestTitleGenerationConcurrentWithMainTurn actually races the two goroutines
// (rather than serializing them in a fixed order) using a start barrier so
// both begin as close to simultaneously as possible, repeated across many
// sessions. Run with -race to catch any data race, and verifies the same
// invariants as TestTitleGenerationDoesNotRaceMainTurnTokens hold regardless
// of true scheduling order.
func TestTitleGenerationConcurrentWithMainTurn(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	sqlDB.SetMaxOpenConns(1) // production-faithful single-connection pool; see TestConcurrentRenameAndUsage_NoDataLoss.
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const (
		mainPromptTokens     = int64(4321)
		mainCompletionTokens = int64(987)
		mainCost             = 0.02
		titleCost            = 0.0005
		iterations           = 50
	)

	for i := 0; i < iterations; i++ {
		sess, err := svc.Create(ctx, "Untitled Session")
		require.NoError(t, err)
		sessionID := sess.ID

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
		)
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, svc.SetUsage(ctx, sessionID, mainPromptTokens, mainCompletionTokens))
			_, err := svc.IncrementCost(ctx, sessionID, mainCost)
			require.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, svc.Rename(ctx, sessionID, "Generated Title"))
			_, err := svc.IncrementCost(ctx, sessionID, titleCost)
			require.NoError(t, err)
		}()

		close(start)
		wg.Wait()

		final, err := svc.Get(ctx, sessionID)
		require.NoError(t, err)

		assert.Equal(t, mainPromptTokens, final.PromptTokens, "iter %d", i)
		assert.Equal(t, mainCompletionTokens, final.CompletionTokens, "iter %d", i)
		assert.InDelta(t, mainCost+titleCost, final.Cost, 1e-9, "iter %d", i)
		assert.Equal(t, "Generated Title", final.Title, "iter %d", i)
	}
}

func countMsgsForSession(t *testing.T, ctx context.Context, sqlDB *sql.DB, sessionID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n))
	return n
}

func countAllSessions(t *testing.T, ctx context.Context, sqlDB *sql.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n))
	return n
}

// TestForkSession_HappyPath verifies the transactional fork clones the
// source session's models, reasoning effort, system prompt, todos, and
// EVERY message into a new session. Reasoning effort is asserted here
// because ForkSession (the web fork button's path) used to silently drop it
// — a divergence from the independently-written CLI fork path, which copied
// reasoning effort but dropped todos. Both entry points now share
// ForkSessionTx, which copies the union of both column sets.
func TestForkSession_HappyPath(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	require.NoError(t, svc.UpdateModels(ctx, src.ID, "provL", "modelL", "provS", "modelS"))
	require.NoError(t, svc.UpdateReasoningEffort(ctx, src.ID, "high", "low"))
	require.NoError(t, svc.UpdateSystemPrompt(ctx, src.ID, "be careful"))
	require.NoError(t, svc.SetTodos(ctx, src.ID, []Todo{{Content: "do thing", Status: TodoStatusPending}}, nil))

	// Seed three messages with distinguishable parts and deterministic order.
	seedRoles := []string{"user", "assistant", "user"}
	seedParts := []string{
		`[{"type":"text","data":{"text":"a"}}]`,
		`[{"type":"text","data":{"text":"b"}}]`,
		`[{"type":"text","data":{"text":"c"}}]`,
	}
	for i := range seedRoles {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), src.ID, seedRoles[i], seedParts[i], 100+i, 100+i)
		require.NoError(t, err)
	}
	require.EqualValues(t, 3, countMsgsForSession(t, ctx, sqlDB, src.ID))

	fork, err := svc.ForkSession(ctx, src.ID, "")
	require.NoError(t, err)
	require.NotEqual(t, src.ID, fork.ID)
	require.Equal(t, "source fork", fork.Title)
	require.Equal(t, "provL", fork.LargeModelProvider)
	require.Equal(t, "modelS", fork.SmallModelID)
	require.Equal(t, "high", fork.LargeModelReasoningEffort)
	require.Equal(t, "low", fork.SmallModelReasoningEffort)
	require.Equal(t, "be careful", fork.SystemPrompt)
	require.Len(t, fork.Todos, 1)
	require.Equal(t, "do thing", fork.Todos[0].Content)

	require.EqualValues(t, 3, countMsgsForSession(t, ctx, sqlDB, fork.ID))

	rows, err := q.ListMessagesBySession(ctx, fork.ID)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, "user", rows[0].Role)
	require.Equal(t, "assistant", rows[1].Role)
	require.Equal(t, "user", rows[2].Role)
	require.Equal(t, seedParts[0], rows[0].Parts)
}

// TestForkSessionTx_AtTruncationAndChild verifies ForkOptions.LimitMsgs
// truncates the copy to the first N messages and ForkOptions.ParentID links
// the fork as a child session — the two CLI-specific knobs (--at, --child)
// that ForkSession's defaults do not exercise.
func TestForkSessionTx_AtTruncationAndChild(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	for i, role := range []string{"user", "assistant", "user", "assistant"} {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)`,
			uuid.NewString(), src.ID, role, 100+i, 100+i)
		require.NoError(t, err)
	}

	fork, count, err := svc.(*service).ForkSessionTx(ctx, src.ID, ForkOptions{
		ParentID:  src.ID,
		LimitMsgs: 2,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.Equal(t, src.ID, fork.ParentSessionID)
	require.EqualValues(t, 2, countMsgsForSession(t, ctx, sqlDB, fork.ID))
}

// TestForkSessionTx_EmptySourceNoLimit verifies forking a source session
// with zero messages succeeds when LimitMsgs is left at its zero value: the
// range check (limit < 1 || limit > len(srcMsgs)) must be skipped for the
// "copy everything" default rather than resolving limit to 0 and then
// rejecting it as out of range. This is the regression covered by the CLI's
// TestForkSessionCLI_EmptySourceNoAt for the underlying shared function.
func TestForkSessionTx_EmptySourceNoLimit(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)
	require.Zero(t, countMsgsForSession(t, ctx, sqlDB, src.ID))

	fork, count, err := svc.(*service).ForkSessionTx(ctx, src.ID, ForkOptions{})
	require.NoError(t, err)
	require.Zero(t, count)
	require.NotEqual(t, src.ID, fork.ID)
}

// TestForkSession_MidwayFailureRollsBack arms a trigger that aborts the
// copy of a sentinel-role message placed AFTER two normal messages, then
// asserts the ENTIRE fork (new session row + every copied message) is rolled
// back and the caller receives the error — no partial fork survives.
func TestForkSession_MidwayFailureRollsBack(t *testing.T) {
	t.Parallel()
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	src, err := svc.Create(ctx, "source")
	require.NoError(t, err)

	// Two normal messages (copied first), then a sentinel message whose role
	// trips the trigger. created_at orders ListMessagesBySession so the
	// sentinel is reached only after the normal copies succeed.
	for i, role := range []string{"user", "assistant", "TRIPWIRE"} {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)`,
			uuid.NewString(), src.ID, role, 100+i, 100+i)
		require.NoError(t, err)
	}

	// Arm the tripwire AFTER seeding the source so seeding is not aborted.
	// The BEFORE INSERT trigger fires (and aborts) only when ForkSession
	// copies the TRIPWIRE message.
	_, err = sqlDB.ExecContext(ctx, `
		CREATE TRIGGER tripwire_before_insert
		BEFORE INSERT ON messages
		WHEN new.role = 'TRIPWIRE'
		BEGIN
			SELECT RAISE(ABORT, 'injected midway copy failure');
		END`)
	require.NoError(t, err)

	beforeSessions := countAllSessions(t, ctx, sqlDB)
	beforeSrcMsgs := countMsgsForSession(t, ctx, sqlDB, src.ID)

	_, err = svc.ForkSession(ctx, src.ID, "should-not-survive")
	require.Error(t, err, "midway copy failure must surface as an error to the caller")

	// No new session row committed: total session count unchanged.
	require.Equal(t, beforeSessions, countAllSessions(t, ctx, sqlDB), "fork session must not survive a rolled-back fork")
	// Source is untouched.
	require.Equal(t, beforeSrcMsgs, countMsgsForSession(t, ctx, sqlDB, src.ID), "source messages must be untouched")
	// No orphan messages reference any non-source session.
	var orphanCount int64
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id != ?`, src.ID).Scan(&orphanCount))
	require.Zero(t, orphanCount, "no partial fork messages may remain after rollback")
}
