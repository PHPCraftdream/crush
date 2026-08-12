-- name: WriteToOrphanOutbox :one
-- Write a call to the orphan outbox when main run queue enqueue fails.
-- Returns the outbox row (or error on write failure).
INSERT INTO orphan_call_outbox (
    id,
    session_id,
    call_data,
    status,
    attempts,
    max_attempts,
    created_at,
    updated_at
) VALUES (?, ?, ?, 'pending', 0, 5, ?, ?)
RETURNING *;

-- name: ListPendingOrphanOutboxEntries :many
-- Get all pending outbox entries for recovery (scanned by pump).
SELECT id, session_id, call_data, status, attempts, max_attempts, last_error, created_at, updated_at
FROM orphan_call_outbox
WHERE status = 'pending'
ORDER BY created_at ASC;

-- name: ClaimOrphanOutboxEntry :one
-- Atomically claim a pending entry for processing (move to processing state).
-- Used by pump when moving an entry to the main run queue.
UPDATE orphan_call_outbox
SET status = 'processing',
    attempts = attempts + 1,
    updated_at = ?
WHERE id = ? AND status = 'pending'
RETURNING *;

-- name: MarkOrphanOutboxEntryDone :execrows
-- Mark an entry as done (successfully moved to main run queue).
DELETE FROM orphan_call_outbox
WHERE id = ? AND status = 'processing';

-- name: MarkOrphanOutboxEntryFailed :one
-- Mark an entry as failed (exhausted retries or persistent error).
UPDATE orphan_call_outbox
SET status = 'failed',
    last_error = ?,
    updated_at = ?
WHERE id = ? AND status = 'processing'
RETURNING *;

-- name: CleanupOldDoneOrphanOutboxEntries :exec
-- Clean up old done entries (optional, for housekeeping).
DELETE FROM orphan_call_outbox
WHERE status = 'done' AND updated_at < ?;

-- name: GetOrphanOutboxEntry :one
-- Get a single entry by ID.
SELECT id, session_id, call_data, status, attempts, max_attempts, last_error, created_at, updated_at
FROM orphan_call_outbox
WHERE id = ?;