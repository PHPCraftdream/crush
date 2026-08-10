-- name: EnqueueRunQueueEntry :one
INSERT INTO session_run_queue (
    id,
    session_id,
    call_data,
    status,
    attempts,
    terminal_failure,
    created_at,
    updated_at
) VALUES (?, ?, ?, 'pending', 0, 0, ?, ?)
RETURNING *;

-- name: GetOldestPendingRunQueueEntryForSession :one
-- Get the oldest pending entry for a session (for transactional lease).
SELECT id, session_id, call_data, status, leased_by, leased_at, lease_expires_at,
       attempts, last_error, terminal_failure, created_at, updated_at
FROM session_run_queue
WHERE session_id = ? AND status = 'pending'
ORDER BY created_at ASC
LIMIT 1;

-- name: LeaseRunQueueEntryByID :one
-- Claim a specific entry by ID (call after GetOldestPendingRunQueueEntryForSession in a transaction).
-- Does not increment attempts: leasing only claims the row for execution.
-- NackRunQueueEntry and CleanupExpiredLeases are the only sites that count
-- an attempt, exactly once per completed failed execution or per lease
-- recovered from a crashed/hung executor. Counting here too, in addition to
-- NackRunQueueEntry, double-counted every failure cycle, silently halving
-- the effective value of RunQueueMaxAttempts.
UPDATE session_run_queue
SET status = 'leased',
    leased_by = ?,
    leased_at = ?,
    lease_expires_at = ?,
    updated_at = ?
WHERE id = ? AND status = 'pending'
RETURNING *;

-- name: AckRunQueueEntry :one
-- Mark a leased entry as successfully completed (terminal).
-- Removes it from the queue (acked is terminal, no longer needed).
DELETE FROM session_run_queue
WHERE id = ? AND status = 'leased'
RETURNING id;

-- name: NackRunQueueEntry :one
-- Release a leased entry back to pending state (non-terminal failure, retry later).
-- Increments attempts but does NOT set terminal_failure.
UPDATE session_run_queue
SET status = 'pending',
    leased_by = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    last_error = ?,
    attempts = attempts + 1,
    updated_at = ?
WHERE id = ? AND status = 'leased'
RETURNING *;

-- name: NackRunQueueEntryNoAttemptPenalty :one
-- Release a leased entry back to pending state without counting it as an
-- attempt. Used specifically for session.SessionLockBusyError: another live
-- process legitimately holding the OS session lock is routine, expected
-- contention, not a failure of the call itself, and must never count toward
-- RunQueueMaxAttempts. Without this, the durable queue would delete
-- accepted user work after nothing more than a few turns of ordinary lock
-- contention.
UPDATE session_run_queue
SET status = 'pending',
    leased_by = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    last_error = ?,
    updated_at = ?
WHERE id = ? AND status = 'leased'
RETURNING *;

-- name: TerminalFailRunQueueEntry :one
-- Mark a leased entry as terminal failure (no retry, even if attempts < max).
-- Used for ErrCallAlreadyAttempted-type errors where retry would cause duplicates.
DELETE FROM session_run_queue
WHERE id = ? AND status = 'leased'
RETURNING id;

-- name: ListPendingRunQueueEntries :many
-- Get all pending entries (for pump scanning across all sessions).
SELECT * FROM session_run_queue
WHERE status = 'pending'
ORDER BY created_at ASC;

-- name: ListStaleLeasedRunQueueEntries :many
-- Get all leased entries with expired leases (for pump lease recovery).
SELECT * FROM session_run_queue
WHERE status = 'leased' AND lease_expires_at < ?
ORDER BY lease_expires_at ASC;

-- name: GetRunQueueEntry :one
-- Get a single entry by ID.
SELECT * FROM session_run_queue
WHERE id = ?;

-- name: CleanupExpiredLeases :exec
-- Reset stale leased entries back to pending (lease expiry recovery).
-- Run periodically to recover from crashed pump instances.
-- Increments attempts: a lease that expired without a matching Ack/Nack
-- means whatever was executing it (a pump process, a goroutine) died or
-- hung mid-execution without ever completing a normal outcome. Without
-- counting this as an attempt, a poison entry whose execution always
-- kills the process before it can Ack or Nack would accumulate attempts=0
-- forever and never reach RunQueueMaxAttempts, looping indefinitely
-- instead of eventually being dead-lettered.
UPDATE session_run_queue
SET status = 'pending',
    leased_by = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    last_error = 'lease_expired',
    attempts = attempts + 1,
    updated_at = ?
WHERE status = 'leased' AND lease_expires_at < ?;