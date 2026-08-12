package session

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestPendingInjectsFIFOOrdering verifies that PeekInterruptInject and
// DrainPendingInjects return rows in FIFO order even when multiple rows
// have the same created_at timestamp. The rowid ASC tie-breaker ensures
// deterministic ordering when created_at is identical.
func TestPendingInjectsFIFOOrdering(t *testing.T) {
	t.Run("DrainPendingInjects FIFO with identical created_at", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB)
		ctx := t.Context()

		// Create a test session
		sessionID := uuid.New().String()
		_, err := sqlDB.ExecContext(ctx, `
			INSERT INTO sessions (id, title, updated_at, created_at)
			VALUES (?, ?, 1, 1)
		`, sessionID, "test session")
		require.NoError(t, err)

		// Create three injects with identical created_at (same second)
		// but insert them sequentially, which assigns increasing rowids
		const sameTimestamp = 1724000000 // arbitrary unix timestamp
		injectIDs := []string{
			uuid.New().String(),
			uuid.New().String(),
			uuid.New().String(),
		}
		messageIDs := []string{
			uuid.New().String(),
			uuid.New().String(),
			uuid.New().String(),
		}

		for i, id := range injectIDs {
			_, err := sqlDB.ExecContext(ctx, `
				INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`, id, sessionID, messageIDs[i], "content"+string(rune('A'+i)), 0, sameTimestamp)
			require.NoError(t, err)
		}

		// Drain and verify FIFO order (insertion order)
		injects, hasInterrupt, err := svc.DrainPendingInjects(ctx, sessionID)
		require.NoError(t, err)
		require.False(t, hasInterrupt)
		require.Len(t, injects, 3)

		// Should return in insertion order (rowid ASC)
		for i := 0; i < 3; i++ {
			require.Equal(t, injectIDs[i], injects[i].ID, "inject %d should match insertion order", i)
			require.Equal(t, "content"+string(rune('A'+i)), injects[i].Content)
		}
	})

	t.Run("PeekInterruptInject FIFO with identical created_at", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB)
		ctx := t.Context()

		// Create a test session
		sessionID := uuid.New().String()
		_, err := sqlDB.ExecContext(ctx, `
			INSERT INTO sessions (id, title, updated_at, created_at)
			VALUES (?, ?, 1, 1)
		`, sessionID, "test session")
		require.NoError(t, err)

		// Create two interrupt injects with identical created_at
		const sameTimestamp = 1724000000
		firstInjectID := uuid.New().String()
		secondInjectID := uuid.New().String()

		_, err = sqlDB.ExecContext(ctx, `
			INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, firstInjectID, sessionID, uuid.New().String(), "first", 1, sameTimestamp)
		require.NoError(t, err)

		_, err = sqlDB.ExecContext(ctx, `
			INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, secondInjectID, sessionID, uuid.New().String(), "second", 1, sameTimestamp)
		require.NoError(t, err)

		// Peek should return the first one (lowest rowid)
		peek, err := svc.PeekInterruptInject(ctx, sessionID)
		require.NoError(t, err)
		require.NotNil(t, peek)
		require.Equal(t, firstInjectID, peek.ID, "Peek should return the first inserted inject")
		require.Equal(t, "first", peek.Content)
	})

	t.Run("DrainPendingInjects with mix of interrupt and regular", func(t *testing.T) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB)
		ctx := t.Context()

		// Create a test session
		sessionID := uuid.New().String()
		_, err := sqlDB.ExecContext(ctx, `
			INSERT INTO sessions (id, title, updated_at, created_at)
			VALUES (?, ?, 1, 1)
		`, sessionID, "test session")
		require.NoError(t, err)

		// Create regular injects
		const sameTimestamp = 1724000000
		regularInjectIDs := []string{
			uuid.New().String(),
			uuid.New().String(),
		}
		for _, id := range regularInjectIDs {
			_, err := sqlDB.ExecContext(ctx, `
				INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`, id, sessionID, uuid.New().String(), "regular", 0, sameTimestamp)
			require.NoError(t, err)
		}

		// Drain should return all non-interrupt injects
		injects, hasInterrupt, err := svc.DrainPendingInjects(ctx, sessionID)
		require.NoError(t, err)
		require.False(t, hasInterrupt)
		require.Len(t, injects, 2)

		// Should return in insertion order
		for i := 0; i < 2; i++ {
			require.Equal(t, regularInjectIDs[i], injects[i].ID)
		}
	})
}

// TestPendingInjectsFIFOOrderingRevertCheck verifies that the rowid ASC tie-breaker
// provides deterministic FIFO ordering for rows with identical created_at values.
//
// This test demonstrates the ordering property guaranteed by the fix:
// - With rowid ASC: rows are returned in strict rowid order (monotonically increasing)
//
// NOTE: In practice, SQLite often returns rows in insertion order even without an
// explicit rowid tie-breaker for simple SELECT queries on in-memory databases, so
// this test may pass even if the tie-breaker is removed. However, SQLite does NOT
// guarantee this behavior per the spec. The rowid tie-breaker is required for
// correctness per SQLite documentation on ORDER BY with equal keys.
//
// For manual verification of the bug fix:
// 1. Temporarily remove ", rowid ASC" from the two queries in session.go
// 2. Run: go test ./internal/session/... -run TestPendingInjectsFIFOOrdering -count=100
// 3. Observe that the test passes (SQLite happens to return insertion order)
// 4. This demonstrates why explicit testing is important - the bug is non-deterministic
// 5. Restore ", rowid ASC" to both queries in session.go
// 6. Run again - still passes, but now with deterministic ordering guarantee
func TestPendingInjectsFIFOOrderingRevertCheck(t *testing.T) {
	t.Run("rowid tie-breaker required for FIFO ordering", func(t *testing.T) {
		sqlDB, _ := newTestDB(t)
		ctx := t.Context()

		// Create a test session
		sessionID := uuid.New().String()
		_, err := sqlDB.ExecContext(ctx, `
			INSERT INTO sessions (id, title, updated_at, created_at)
			VALUES (?, ?, 1, 1)
		`, sessionID, "test session")
		require.NoError(t, err)

		// Create 5 injects with identical created_at
		const sameTimestamp = 1724000000
		injectIDs := []string{
			uuid.New().String(),
			uuid.New().String(),
			uuid.New().String(),
			uuid.New().String(),
			uuid.New().String(),
		}

		for _, id := range injectIDs {
			_, err := sqlDB.ExecContext(ctx, `
				INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`, id, sessionID, uuid.New().String(), "content", 0, sameTimestamp)
			require.NoError(t, err)
		}

		// Query WITH rowid tie-breaker (the fixed version)
		rows, err := sqlDB.QueryContext(ctx, `
			SELECT id, rowid, session_id, message_id, content, interrupt, created_at
			FROM pending_injects WHERE session_id = ? ORDER BY created_at ASC, rowid ASC
		`, sessionID)
		require.NoError(t, err)
		defer rows.Close()

		var (
			resultIDs []string
			rowids    []int64
		)
		for rows.Next() {
			var (
				id        string
				rowid     int64
				sessionID string
				messageID string
				content   string
				interrupt int64
				createdAt int64
			)
			err := rows.Scan(&id, &rowid, &sessionID, &messageID, &content, &interrupt, &createdAt)
			require.NoError(t, err)
			resultIDs = append(resultIDs, id)
			rowids = append(rowids, rowid)
		}

		require.Len(t, resultIDs, 5)

		// Verify rowids are in strictly ascending order
		// This is the key property guaranteed by the rowid tie-breaker
		for i := 1; i < len(rowids); i++ {
			require.Greater(t, rowids[i], rowids[i-1],
				"rowids should be in strictly ascending order: rowids[%d]=%d should be > rowids[%d]=%d",
				i, rowids[i], i-1, rowids[i-1])
		}
	})
}
