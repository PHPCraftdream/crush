-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS session_permissions (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    action TEXT NOT NULL,
    path TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_session_permissions_session_id ON session_permissions (session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_session_permissions_session_id;
DROP TABLE IF EXISTS session_permissions;
-- +goose StatementEnd
