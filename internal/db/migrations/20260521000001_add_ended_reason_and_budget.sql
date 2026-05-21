-- +goose Up
-- Fork patch: ended_reason + budget columns for operator UX.
-- ended_reason: "done", "canceled", "timeout", "max_cost", "max_tokens",
--   "error", "crash" (set by orphan-PID detection), or "" (not yet ended).
-- budget_max_cost / budget_max_tokens / budget_timeout_sec: persisted from
--   --max-cost / --max-tokens / --timeout so sessions show/locks can display
--   "cost vs budget".
ALTER TABLE sessions ADD COLUMN ended_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN budget_max_cost REAL NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN budget_max_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN budget_timeout_sec INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35.0; these are safe no-ops
-- on older versions. On 3.35+ they work fine.
ALTER TABLE sessions DROP COLUMN ended_reason;
ALTER TABLE sessions DROP COLUMN budget_max_cost;
ALTER TABLE sessions DROP COLUMN budget_max_tokens;
ALTER TABLE sessions DROP COLUMN budget_timeout_sec;
