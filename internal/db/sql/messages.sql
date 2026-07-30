-- name: GetMessage :one
SELECT *
FROM messages
WHERE id = ? LIMIT 1;

-- name: ListMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ?
-- rowid is the tie-breaker: created_at is stored in SECONDS, so a single agent
-- turn produces dozens of rows with an identical created_at. Without a total
-- order SQLite does not guarantee a stable order among those ties. This is the
-- same class of bug fixed for ListMessagesBySessionPaginated (see its comment
-- below) - here applied to the oldest-first, non-paginated variant, so
-- (created_at ASC, rowid ASC) is a deterministic oldest-first total order.
-- rowid is SQLite's implicit monotonic insertion counter (messages.id is a
-- non-monotonic UUID, unsuitable as a tiebreaker).
ORDER BY created_at ASC, rowid ASC;

-- name: CreateMessage :one
INSERT INTO messages (
    id,
    session_id,
    role,
    parts,
    model,
    provider,
    reasoning_effort,
    is_summary_message,
    hidden,
    auto_resumed,
    background_job_notice,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: UpdateMessage :exec
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: UpdateMessagePinned :exec
UPDATE messages
SET
    pinned = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: DeleteMessage :exec
DELETE FROM messages
WHERE id = ?;

-- name: DeleteSessionMessages :exec
DELETE FROM messages
WHERE session_id = ?;

-- name: ListUserMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ? AND role = 'user'
-- rowid is the tie-breaker: created_at is stored in SECONDS, so multiple user
-- messages within the same second (e.g. rapid follow-ups) are not given a
-- stable order by created_at alone. Same class of bug fixed for
-- ListMessagesBySession/ListMessagesBySessionPaginated - rowid is SQLite's
-- implicit monotonic insertion counter (messages.id is a non-monotonic UUID,
-- unsuitable as a tiebreaker), so (created_at DESC, rowid DESC) is a
-- deterministic newest-first total order.
ORDER BY created_at DESC, rowid DESC;

-- name: ListAllUserMessages :many
SELECT *
FROM messages
WHERE role = 'user'
-- rowid is the tie-breaker: see ListUserMessagesBySession above - identical
-- reasoning applies across all sessions, not just one.
ORDER BY created_at DESC, rowid DESC;

-- name: ListMessagesBySessionPaginated :many
SELECT *
FROM messages
WHERE session_id = ?
-- rowid is the tie-breaker: created_at is stored in SECONDS, so a single agent
-- turn produces dozens of rows with an identical created_at. Without a total
-- order SQLite does not guarantee a stable order among those ties, which makes
-- OFFSET pagination lose/duplicate rows when the query plan shifts between
-- page fetches. rowid is SQLite's implicit monotonic insertion counter, so
-- (created_at DESC, rowid DESC) is a deterministic newest-first total order.
ORDER BY created_at DESC, rowid DESC
LIMIT ? OFFSET ?;

-- name: CountMessagesBySession :one
SELECT COUNT(*)
FROM messages
WHERE session_id = ?;
