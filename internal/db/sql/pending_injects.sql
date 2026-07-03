-- pending_injects is the cross-process inject queue for `crush sessions
-- inject`. See migration 20260703000001 for the full semantics.
--
-- NOTE: as of this fork the session-layer wrapper (session.go
-- DrainPendingInjects / CreatePendingInject) talks to this table via raw
-- database/sql, matching the existing cross-process signal pattern
-- (RequestCancel / SetBudget). These sqlc annotations are kept so a future
-- `sqlc generate` stays consistent, but the generated methods are not
-- currently wired into db.go to avoid sqlc rewriting unrelated files.

-- name: CreatePendingInject :exec
INSERT INTO pending_injects (
    id,
    session_id,
    message_id,
    content,
    interrupt,
    created_at
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?
);

-- name: ListPendingInjectsBySession :many
SELECT * FROM pending_injects
WHERE session_id = ?
ORDER BY created_at ASC;

-- name: DeletePendingInject :exec
DELETE FROM pending_injects
WHERE id = ?;
