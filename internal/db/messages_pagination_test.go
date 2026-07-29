package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// pagDialectOnce guards the global goose dialect set. The db package's init()
// already calls goose.SetBaseFS(FS); the dialect is normally set lazily by
// initGoose() (sync.Once) when Connect runs. These tests use openDB + goose.Up
// directly rather than Connect, so they must ensure the dialect is set, but
// calling goose.SetDialect from t.Parallel() tests races on the global. We
// therefore set it once, serialized, and keep these tests non-parallel to
// avoid adding pressure to the package's pre-existing global-goose race.

const pagFixedCreatedAt = int64(1700000000)

// setupPaginationDB opens a fresh SQLite database and applies all migrations,
// mirroring the pattern in cost_accounting_test.go.
var pagDialectOnce sync.Once

func setupPaginationDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/pag.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	pagDialectOnce.Do(func() {
		require.NoError(t, goose.SetDialect("sqlite3"))
	})
	require.NoError(t, goose.Up(conn, "migrations"))
	return ctx, conn
}

func insertPagSession(t *testing.T, ctx context.Context, conn *sql.DB, id string) {
	t.Helper()
	_, err := conn.ExecContext(ctx,
		`INSERT INTO sessions (id, title, updated_at, created_at)
		 VALUES (?, ?, ?, ?)`,
		id, id, pagFixedCreatedAt, pagFixedCreatedAt,
	)
	require.NoError(t, err)
}

func insertPagMessage(t *testing.T, ctx context.Context, conn *sql.DB, sessionID, id string) {
	t.Helper()
	_, err := conn.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
		 VALUES (?, ?, 'user', '[]', ?, ?)`,
		id, sessionID, pagFixedCreatedAt, pagFixedCreatedAt,
	)
	require.NoError(t, err)
}

// pagExpectedDesc builds the expected newest-first id sequence for n messages
// inserted as msg-00..msg-(n-1): a deterministic rowid tie-breaker yields
// insertion-descending order (msg-(n-1) .. msg-00).
func pagExpectedDesc(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("msg-%02d", n-1-i)
	}
	return out
}

// TestListMessagesBySessionPaginated_IdenticalCreatedAt pages through a session
// whose messages ALL share one created_at (the real-world norm for a single
// agent turn, since created_at is stored in SECONDS). Without a deterministic
// tie-breaker SQLite does not guarantee a stable order among same-second rows,
// so OFFSET pagination can duplicate or skip them. The test asserts both
// completeness (every message returned exactly once) and a deterministic order.
func TestListMessagesBySessionPaginated_IdenticalCreatedAt(t *testing.T) {
	ctx, conn := setupPaginationDB(t)
	q := New(conn)

	const target = "sess-target"
	insertPagSession(t, ctx, conn, target)
	// Other sessions with their own messages steer SQLite toward the
	// session_id index for the target query, which surfaces the implicit
	// (un-guaranteed) tie order most clearly.
	for _, sid := range []string{"sess-a", "sess-b", "sess-c"} {
		insertPagSession(t, ctx, conn, sid)
		for i := 0; i < 5; i++ {
			insertPagMessage(t, ctx, conn, sid, fmt.Sprintf("%s-m%02d", sid, i))
		}
	}

	const n = 30
	for i := 0; i < n; i++ {
		insertPagMessage(t, ctx, conn, target, fmt.Sprintf("msg-%02d", i))
	}

	// Page size 7 does not evenly divide 30, forcing a partial final page.
	const pageSize = 7
	var got []string
	seen := make(map[string]int)
	for offset := 0; ; offset += pageSize {
		rows, err := q.ListMessagesBySessionPaginated(ctx, ListMessagesBySessionPaginatedParams{
			SessionID: target,
			Limit:     pageSize,
			Offset:    int64(offset),
		})
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			got = append(got, r.ID)
			seen[r.ID]++
		}
	}

	t.Logf("paged id sequence (newest-first): %v", got)

	require.Len(t, got, n, "must return all %d messages across pages", n)
	var dups, missing []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("msg-%02d", i)
		switch seen[id] {
		case 0:
			missing = append(missing, id)
		case 1:
			// ok
		default:
			dups = append(dups, id)
		}
	}
	require.Empty(t, missing, "messages missing (skipped) across pages: %v", missing)
	require.Empty(t, dups, "messages duplicated across pages: %v", dups)

	require.Equal(t, pagExpectedDesc(n), got,
		"paginated order must be deterministic (created_at DESC, rowid DESC)")
}

// TestListMessagesBySessionPaginated_TieBreakerOrder isolates the tie-breaker
// ordering from paging: a single fetch of all same-second messages must come
// back in deterministic rowid-descending (insertion-descending) order.
func TestListMessagesBySessionPaginated_TieBreakerOrder(t *testing.T) {
	ctx, conn := setupPaginationDB(t)
	q := New(conn)

	const target = "sess-target"
	insertPagSession(t, ctx, conn, target)
	for _, sid := range []string{"sess-a", "sess-b"} {
		insertPagSession(t, ctx, conn, sid)
		for i := 0; i < 5; i++ {
			insertPagMessage(t, ctx, conn, sid, fmt.Sprintf("%s-m%02d", sid, i))
		}
	}
	const n = 30
	for i := 0; i < n; i++ {
		insertPagMessage(t, ctx, conn, target, fmt.Sprintf("msg-%02d", i))
	}

	rows, err := q.ListMessagesBySessionPaginated(ctx, ListMessagesBySessionPaginatedParams{
		SessionID: target,
		Limit:     100,
		Offset:    0,
	})
	require.NoError(t, err)
	require.Len(t, rows, n)

	got := make([]string, n)
	for i, r := range rows {
		got[i] = r.ID
	}
	t.Logf("single-fetch id sequence (newest-first): %v", got)
	require.Equal(t, pagExpectedDesc(n), got,
		"order must be deterministic (created_at DESC, rowid DESC)")
}
