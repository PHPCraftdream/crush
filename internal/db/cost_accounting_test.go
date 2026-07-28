package db

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParentCostAccountedBackfill verifies the ledger migration's backfill
// step (required test г). A session that existed BEFORE the migration — with
// cost > 0 and no parent_cost_accounted column — must, once the migration is
// applied, end up with parent_cost_accounted == cost. Without that backfill
// the ADD COLUMN ... DEFAULT 0 would leave the pre-existing row accounted at
// 0, and the first post-upgrade TransferChildCostToParent call would
// re-charge the parent for the entire accumulated cost a second time (the
// migration itself creating the double-charge it is meant to prevent).
//
// The test drives goose directly: it applies every migration EXCEPT the
// ledger one, inserts a pre-migration row, then applies the ledger migration
// and asserts the backfill UPDATE corrected the row.
func TestParentCostAccountedBackfill(t *testing.T) {
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/backfill.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// goose state is package-global (SetBaseFS in init, dialect in initGoose);
	// restate both idempotently to be safe in isolation.
	goose.SetBaseFS(FS)
	require.NoError(t, goose.SetDialect("sqlite3"))

	// Apply every migration EXCEPT the parent_cost_accounted ledger one (the
	// current highest version). UpTo is inclusive of the given version.
	const beforeLedger int64 = 20260703000001
	require.NoError(t, goose.UpTo(conn, "migrations", beforeLedger))

	// Insert a session as it would exist BEFORE the ledger migration: cost >
	// 0 and no parent_cost_accounted column (it does not exist yet).
	_, err = conn.ExecContext(ctx,
		`INSERT INTO sessions (id, title, cost, updated_at, created_at)
		 VALUES (?, ?, ?, strftime('%s','now'), strftime('%s','now'))`,
		"pre-migration-child", "child", 0.07,
	)
	require.NoError(t, err)

	// Apply the remaining migrations: the ledger one adds the column with
	// DEFAULT 0 (so the pre-existing row starts at accounted 0) and then runs
	// the backfill UPDATE setting accounted = cost.
	require.NoError(t, goose.Up(conn, "migrations"))

	// The backfill must have corrected the pre-existing row: accounted now
	// equals cost, so the first post-upgrade transfer charges zero delta.
	var cost, accounted float64
	err = conn.QueryRowContext(ctx,
		`SELECT cost, parent_cost_accounted FROM sessions WHERE id = ?`,
		"pre-migration-child",
	).Scan(&cost, &accounted)
	require.NoError(t, err)
	assert.InDelta(t, 0.07, cost, 1e-9)
	assert.InDelta(t, 0.07, accounted, 1e-9,
		"backfill must set parent_cost_accounted == cost for pre-existing rows; otherwise the first transfer double-charges the parent")

	// A row created AFTER the migration with cost 0 keeps accounted 0 (the
	// DEFAULT), which is correct — there is nothing to account for yet.
	_, err = conn.ExecContext(ctx,
		`INSERT INTO sessions (id, title, cost, updated_at, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'), strftime('%s','now'))`,
		"post-migration-zero", "zero",
	)
	require.NoError(t, err)
	var zeroAccounted float64
	err = conn.QueryRowContext(ctx,
		`SELECT parent_cost_accounted FROM sessions WHERE id = ?`,
		"post-migration-zero",
	).Scan(&zeroAccounted)
	require.NoError(t, err)
	assert.InDelta(t, 0.0, zeroAccounted, 1e-9, "a fresh zero-cost session must default to accounted 0")
}
