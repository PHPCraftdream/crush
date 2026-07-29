package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// newCallTreeTestDB opens a fresh file-backed SQLite database with every
// real migration applied (via db.Connect, same path production uses), so
// these tests exercise the ACTUAL current schema rather than a hand-rolled
// approximation that can drift from it (see the fork's CLAUDE.md note on
// the sqlc-generated-code drift this task uncovered).
func newCallTreeTestDB(t *testing.T) (*sql.DB, *Queries) {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Release(dataDir) })
	return conn, New(conn)
}

// insertSession inserts a minimal session row with caller-controlled
// timestamps and parent_session_id, bypassing sqlc's CreateSession (which
// always stamps strftime('%s','now')) so tests can build deterministic
// trees with precise, ordered timestamps.
func insertSession(t *testing.T, conn *sql.DB, id, parentID string, createdAt, updatedAt int64) {
	t.Helper()
	var parent sql.NullString
	if parentID != "" {
		parent = sql.NullString{String: parentID, Valid: true}
	}
	_, err := conn.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, parent, "test session", updatedAt, createdAt,
	)
	require.NoError(t, err)
}

// insertMessage inserts a minimal message row with caller-controlled
// timestamps, bypassing sqlc's CreateMessage (which always stamps
// strftime('%s','now')) for the same determinism reason as insertSession.
func insertMessage(t *testing.T, conn *sql.DB, sessionID, role string, createdAt, updatedAt int64) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(),
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
		 VALUES (?, ?, ?, '[]', ?, ?)`,
		uuid.NewString(), sessionID, role, createdAt, updatedAt,
	)
	require.NoError(t, err)
}

// TestGetCallTreeActivity_DeepTree builds root -> child1 -> grandchild and
// root -> child2, with the freshest message on the grandchild (three levels
// down), and asserts the recursive CTE finds it — proving the query walks
// the FULL tree depth, not just direct children.
func TestGetCallTreeActivity_DeepTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	root := "root-" + uuid.NewString()
	child1 := "child1-" + uuid.NewString()
	child2 := "child2-" + uuid.NewString()
	grandchild := "grandchild-" + uuid.NewString()

	insertSession(t, conn, root, "", 100, 100)
	insertSession(t, conn, child1, root, 100, 100)
	insertSession(t, conn, child2, root, 100, 100)
	insertSession(t, conn, grandchild, child1, 100, 100)

	insertMessage(t, conn, root, "user", 100, 100)
	insertMessage(t, conn, child2, "assistant", 200, 200)
	// Freshest activity: three levels deep, on the grandchild.
	insertMessage(t, conn, grandchild, "tool", 999, 999)

	row, err := q.GetCallTreeActivity(ctx, root)
	require.NoError(t, err)
	require.Equal(t, grandchild, row.SessionID, "must find activity on the grandchild, not just direct children")
	require.Equal(t, "tool", row.Role)
	require.EqualValues(t, 999, row.LatestUnix)
}

// TestGetCallTreeActivity_UsesUpdatedAtWhenNewer asserts the query picks
// MAX(created_at, updated_at), matching the old Go latestMessageUnix helper:
// a message whose updated_at was bumped by a later edit must win over a
// different message's created_at, even though the edited message's own
// created_at is older.
func TestGetCallTreeActivity_UsesUpdatedAtWhenNewer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	root := "root-" + uuid.NewString()
	insertSession(t, conn, root, "", 100, 100)

	// Older message, but edited (updated_at bumped) after the newer-created one.
	_, err := conn.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, 'assistant', '[]', 100, 500)`,
		uuid.NewString(), root,
	)
	require.NoError(t, err)
	// Newer-created message, never edited.
	_, err = conn.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?, ?, 'user', '[]', 300, 300)`,
		uuid.NewString(), root,
	)
	require.NoError(t, err)

	row, err := q.GetCallTreeActivity(ctx, root)
	require.NoError(t, err)
	require.EqualValues(t, 500, row.LatestUnix, "must use the edited message's updated_at, not just created_at")
	require.Equal(t, "assistant", row.Role)
}

// TestGetCallTreeActivity_TieBreakPrefersDescendant is the regression test
// for the tie-break rule carried over from the old Go BFS implementation
// (see the "tie-break toward descendants" comment on the pre-SQL
// computeCallTreeActivity): when the root and a descendant have activity at
// the EXACT SAME timestamp, the descendant must win, so a fast delegation
// reads as "sub-agent active" rather than "parent active" for its first
// second.
func TestGetCallTreeActivity_TieBreakPrefersDescendant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	root := "root-" + uuid.NewString()
	child := "child-" + uuid.NewString()
	insertSession(t, conn, root, "", 100, 100)
	insertSession(t, conn, child, root, 100, 100)

	// Root and child both have activity at the exact same timestamp.
	insertMessage(t, conn, root, "assistant", 500, 500)
	insertMessage(t, conn, child, "assistant", 500, 500)

	row, err := q.GetCallTreeActivity(ctx, root)
	require.NoError(t, err)
	require.Equal(t, child, row.SessionID, "on a timestamp tie, the descendant must win over the root")
}

// TestGetCallTreeActivity_TieBreakPrefersDeeperDescendant strengthens the
// tie-break rule one step further: when TWO descendants at different depths
// tie on timestamp, the deeper one wins. This is a reasonable, deterministic
// extension of "prefer descendant" now that the whole tree is ranked in one
// query (the old Go BFS's behaviour for descendant-vs-descendant ties was an
// unspecified artifact of queue order, not a documented contract).
func TestGetCallTreeActivity_TieBreakPrefersDeeperDescendant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	root := "root-" + uuid.NewString()
	child := "child-" + uuid.NewString()
	grandchild := "grandchild-" + uuid.NewString()
	insertSession(t, conn, root, "", 100, 100)
	insertSession(t, conn, child, root, 100, 100)
	insertSession(t, conn, grandchild, child, 100, 100)

	insertMessage(t, conn, child, "assistant", 500, 500)
	insertMessage(t, conn, grandchild, "tool", 500, 500)

	row, err := q.GetCallTreeActivity(ctx, root)
	require.NoError(t, err)
	require.Equal(t, grandchild, row.SessionID, "on a timestamp tie between two descendants, the deeper one must win")
}

// TestGetCallTreeActivity_EmptyTree asserts a session with no messages
// anywhere in its tree returns sql.ErrNoRows (the Go wrapper,
// session.Service.GetCallTreeActivity, turns this into ok=false).
func TestGetCallTreeActivity_EmptyTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	root := "root-" + uuid.NewString()
	insertSession(t, conn, root, "", 100, 100)

	_, err := q.GetCallTreeActivity(ctx, root)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestGetCallTreeActivity_DepthGuard builds a session chain deeper than the
// 511-hop cap baked into the recursive CTE and asserts the query still
// returns promptly (no hang) and simply does not see activity beyond the
// cap. This guards the DEPTH axis of the recursion; tree WIDTH (fan-out) is
// intentionally unbounded -- see call_tree_activity.sql's header comment.
//
// SQLite session/message rows have no FK-based way to form a genuine CYCLE
// in parent_session_id (a chain always terminates at a NULL parent), so the
// realistic failure mode this guards against is an extremely deep or wide
// tree, not a true cycle. This test exercises the depth side of that guard.
func TestGetCallTreeActivity_DepthGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	const chainLen = 600 // comfortably past the 511-hop cap
	root := "root-" + uuid.NewString()
	insertSession(t, conn, root, "", 100, 100)

	prev := root
	for i := 0; i < chainLen; i++ {
		id := uuid.NewString()
		insertSession(t, conn, id, prev, 100, 100)
		prev = id
	}
	// Put the freshest message on the LAST (deepest, beyond-cap) session.
	insertMessage(t, conn, prev, "assistant", 999999, 999999)
	// And a findable one within the cap, near the root.
	insertMessage(t, conn, root, "user", 100, 100)

	row, err := q.GetCallTreeActivity(ctx, root)
	require.NoError(t, err, "query must return promptly instead of hanging on a deep chain")
	// The beyond-cap message must NOT be visible; only the in-cap root
	// message is found.
	require.Equal(t, root, row.SessionID)
	require.EqualValues(t, 100, row.LatestUnix)
}

// TestGetCallTreeActivityBatch_IsolatesRoots builds two independent trees
// (each with its own child) plus a root with no messages, and asserts the
// batch query returns each root's OWN answer without leaking another root's
// activity into it — the core correctness property of the batch form.
func TestGetCallTreeActivityBatch_IsolatesRoots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	rootA := "rootA-" + uuid.NewString()
	childA := "childA-" + uuid.NewString()
	rootB := "rootB-" + uuid.NewString()
	childB := "childB-" + uuid.NewString()
	rootEmpty := "rootEmpty-" + uuid.NewString()

	insertSession(t, conn, rootA, "", 100, 100)
	insertSession(t, conn, childA, rootA, 100, 100)
	insertSession(t, conn, rootB, "", 100, 100)
	insertSession(t, conn, childB, rootB, 100, 100)
	insertSession(t, conn, rootEmpty, "", 100, 100)

	insertMessage(t, conn, rootA, "user", 100, 100)
	insertMessage(t, conn, childA, "assistant", 700, 700) // A's freshest
	insertMessage(t, conn, rootB, "user", 100, 100)
	insertMessage(t, conn, childB, "assistant", 300, 300) // B's freshest

	rows, err := q.GetCallTreeActivityBatch(ctx, []string{rootA, rootB, rootEmpty})
	require.NoError(t, err)

	byRoot := make(map[string]GetCallTreeActivityBatchRow, len(rows))
	for _, r := range rows {
		byRoot[r.RootSessionID] = r
	}

	require.Contains(t, byRoot, rootA)
	require.Equal(t, childA, byRoot[rootA].SessionID)
	require.EqualValues(t, 700, byRoot[rootA].LatestUnix)

	require.Contains(t, byRoot, rootB)
	require.Equal(t, childB, byRoot[rootB].SessionID)
	require.EqualValues(t, 300, byRoot[rootB].LatestUnix)

	require.NotContains(t, byRoot, rootEmpty, "a root with no activity anywhere in its tree must be absent from the batch result")
}

// TestGetCallTreeActivityBatch_EmptyInput asserts an empty root list returns
// an empty (not nil-panicking, not erroring) result set.
func TestGetCallTreeActivityBatch_EmptyInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, q := newCallTreeTestDB(t)

	rows, err := q.GetCallTreeActivityBatch(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestGetCallTreeActivity_WideTree is the WIDTH counterpart of the depth
// guard above: one root with a large number of DIRECT children (depth 1, but
// wide fan-out). Since fan-out is intentionally NOT bounded by row count (see
// call_tree_activity.sql's header comment), this test asserts the query still
// completes promptly and returns the correct freshest activity -- confirming
// that an unbounded fan-out is not a real problem at a realistic-but-large
// width. Contrast with TestGetCallTreeActivity_DepthGuard which exercises the
// DEPTH axis.
func TestGetCallTreeActivity_WideTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, q := newCallTreeTestDB(t)

	const childCount = 2000
	root := "root-" + uuid.NewString()
	insertSession(t, conn, root, "", 100, 100)
	insertMessage(t, conn, root, "user", 100, 100)

	// Bulk-insert via a transaction: the pure-Go SQLite driver is far faster
	// committing one tx than childCount*2 individual Exec round-trips.
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	for i := 0; i < childCount; i++ {
		id := uuid.NewString()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
			 VALUES (?, ?, 'wide', 100, 100)`,
			id, root,
		)
		require.NoError(t, err)
		// Each child's activity is strictly increasing; the last child wins.
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
			 VALUES (?, ?, 'assistant', '[]', ?, ?)`,
			uuid.NewString(), id, int64(200+i), int64(200+i),
		)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	row, err := q.GetCallTreeActivity(ctx, root)
	require.NoError(t, err, "wide fan-out must not hang or error")
	require.EqualValues(t, 200+childCount-1, row.LatestUnix, "freshest activity is on the last child")
}

// TestMigration_ParentSessionIdIndex asserts migration 20260728000002 created
// idx_sessions_parent_session_id on sessions(parent_session_id). That index
// backs the recursive descent in GetCallTreeActivity
// (WHERE s.parent_session_id = tree.session_id) and ListSubSessions; without
// it every recursion step is a full sessions scan. newCallTreeTestDB runs the
// REAL migrations via db.Connect, so this verifies the migration applies
// cleanly and produces the index.
func TestMigration_ParentSessionIdIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, _ := newCallTreeTestDB(t)

	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='sessions'`)
	require.NoError(t, err)
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	require.Contains(t, names, "idx_sessions_parent_session_id",
		"migration must create idx_sessions_parent_session_id on sessions(parent_session_id)")
}
