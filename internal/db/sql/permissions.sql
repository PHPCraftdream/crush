-- name: CreateSessionPermission :exec
INSERT INTO session_permissions (id, session_id, tool_name, action, path, enabled)
VALUES (?, ?, ?, ?, ?, 1);

-- name: ListAllSessionPermissions :many
SELECT id, session_id, tool_name, action, path, created_at, enabled
FROM session_permissions;

-- name: ListSessionPermissions :many
SELECT id, session_id, tool_name, action, path, created_at, enabled
FROM session_permissions
WHERE session_id = ?;

-- name: UpdatePermissionEnabled :exec
UPDATE session_permissions
SET enabled = ?
WHERE id = ?;

-- name: DeletePermission :exec
DELETE FROM session_permissions
WHERE id = ?;
