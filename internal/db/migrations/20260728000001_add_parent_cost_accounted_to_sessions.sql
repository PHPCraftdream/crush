-- +goose Up
-- Persisted ledger for sub-agent cost accounting. parent_cost_accounted
-- records how much of a child session's cost has already been charged to
-- its parent; the coordinator's TransferChildCostToParent reads it inside
-- a transaction so only the delta since the last charge is applied.
ALTER TABLE sessions ADD COLUMN parent_cost_accounted REAL NOT NULL DEFAULT 0;
-- Backfill for existing rows: their cost was already charged to parents
-- under the old in-memory baseline scheme, so mark it fully accounted-for.
-- Without this step the first post-upgrade resume would re-charge the full
-- accumulated total a second time (the migration itself would create the
-- double-charge it is meant to prevent). New rows start at cost=0, so
-- DEFAULT 0 stays correct for sessions created after this migration.
UPDATE sessions SET parent_cost_accounted = cost WHERE cost > 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN parent_cost_accounted;
