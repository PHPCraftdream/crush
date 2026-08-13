-- Task #340's original claim/mark-done/mark-failed/release-for-retry model
-- (ClaimOrphanOutboxEntry, MarkOrphanOutboxEntryDone, MarkOrphanOutboxEntryFailed,
-- ReleaseOrphanOutboxEntryForRetry, CleanupOldDoneOrphanOutboxEntries) was
-- superseded by task #426's atomic DrainOrphanOutboxEntry (single
-- insert-to-main-queue + delete-from-outbox transaction, no intermediate
-- 'processing'/'done'/'failed' state to get stuck in). Removed as dead
-- code -- task #440 follow-up decision: a genuinely malformed,
-- never-enqueueable entry now retries every drain tick forever instead of
-- reaching a terminal 'failed' state. Accepted deliberately rather than
-- reintroducing attempts-tracking into the atomic transaction: the FK
-- ON DELETE CASCADE on session_id already closes the realistic failure
-- mode (session deleted -> row cascades away on its own); what's left is
-- an operationally-visible (slog.Error per tick), not silent, edge case
-- for data that was malformed from the start. `attempts`/`max_attempts`/
-- `status` values other than 'pending' are consequently unreachable going
-- forward but left in the schema rather than a migration for this.

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

-- name: GetOrphanOutboxEntry :one
-- Get a single entry by ID.
SELECT id, session_id, call_data, status, attempts, max_attempts, last_error, created_at, updated_at
FROM orphan_call_outbox
WHERE id = ?;

-- name: DeleteOrphanOutboxEntryIfPending :execrows
-- Delete an orphan outbox entry only if it's still pending (for atomic drain).
-- Returns the number of rows deleted (0 or 1).
-- Used by DrainOrphanOutboxEntry to atomically move entries to the main queue.
DELETE FROM orphan_call_outbox
WHERE id = ? AND status = 'pending';