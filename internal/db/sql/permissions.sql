-- name: CreateSessionPermission :exec
-- ON CONFLICT DO NOTHING relies on idx_session_permissions_uniq from
-- migration 20260517000001. Without it a repeated Always-Allow click
-- on the same (session, tool, action, path) tuple would create a fresh
-- row instead of being a no-op. Re-enabling a disabled-then-regranted
-- rule is handled explicitly via UpdatePermissionEnabled, not here.
INSERT INTO session_permissions (id, session_id, tool_name, action, path, enabled)
VALUES (?, ?, ?, ?, ?, 1)
ON CONFLICT(session_id, tool_name, action, path) DO NOTHING;

-- name: ListAllSessionPermissions :many
SELECT id, session_id, tool_name, action, path, created_at, enabled
FROM session_permissions;

-- name: ListSessionPermissions :many
SELECT id, session_id, tool_name, action, path, created_at, enabled
FROM session_permissions
WHERE session_id = ?;

-- name: MatchSessionPermission :one
-- Returns the row id of an enabled "always allow" rule that matches the
-- given (sessionID, toolName, action, path) tuple, or sql.ErrNoRows.
-- session_id is empty for global rules; we accept either empty or the
-- exact session_id so the same query handles both.
SELECT id
FROM session_permissions
WHERE enabled = 1
  AND tool_name = ?
  AND action = ?
  AND path = ?
  AND (session_id = '' OR session_id = ?)
LIMIT 1;

-- name: UpdatePermissionEnabled :exec
UPDATE session_permissions
SET enabled = ?
WHERE id = ?;

-- name: DeletePermission :exec
DELETE FROM session_permissions
WHERE id = ?;
