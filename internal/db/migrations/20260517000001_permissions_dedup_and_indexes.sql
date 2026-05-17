-- Fork patch (concurrency): see CHANGELOG.fork.md Section 4.I.
-- Two goals:
--   (1) Prevent duplicate "always allow" rows. After the in-memory
--       permission cache was removed, every GrantPersistent click that
--       matched an already-granted (tool, action, path) was creating a
--       fresh row instead of being a no-op. Add a UNIQUE index so the
--       new INSERT ... ON CONFLICT DO NOTHING in permissions.sql works.
--   (2) Make MatchSessionPermission's WHERE clause index-backed.
--       Without a composite index on (tool_name, action, path, enabled)
--       the SELECT in the auto-approve hot path was a full table scan
--       — cheap at N=tens but degrades under N=hundreds across 5+
--       parallel processes.
--
-- +goose Up
-- +goose StatementBegin
DELETE FROM session_permissions
WHERE id NOT IN (
    SELECT MIN(id) FROM session_permissions
    GROUP BY session_id, tool_name, action, path
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_session_permissions_uniq
    ON session_permissions (session_id, tool_name, action, path);
CREATE INDEX IF NOT EXISTS idx_session_permissions_match
    ON session_permissions (tool_name, action, path, enabled);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_session_permissions_match;
DROP INDEX IF EXISTS idx_session_permissions_uniq;
-- +goose StatementEnd
