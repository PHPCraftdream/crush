-- +goose Up
-- +goose StatementBegin
-- Index backing the recursive-CTE descent in call_tree_activity.sql
-- (GetCallTreeActivity / GetCallTreeActivityBatch recurse via
-- WHERE s.parent_session_id = tree.session_id) and ListSubSessions
-- (WHERE parent_session_id = ?). Without it each recursion step is a full
-- table scan of sessions, which matters for `sessions list`/`why`/`locks`
-- that fan this query out across every running root.
CREATE INDEX IF NOT EXISTS idx_sessions_parent_session_id
    ON sessions (parent_session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_parent_session_id;
-- +goose StatementEnd
