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

-- name: RenewRunQueueLease :execrows
-- Extend a lease expiry while its owner is still genuinely working on it,
-- or to back off a lease without counting a failed attempt. Scoped to
-- status = leased AND leased_by = ? so a lease already reassigned to a
-- different owner (this owner lost the race to a prior CleanupExpiredLeases
-- recovery) is never silently extended out from under the new owner:
-- execrows reports 0 rows in that case, which the caller must treat as
-- meaning it is no longer this executor's lease to keep alive.
UPDATE session_run_queue
SET lease_expires_at = ?
WHERE id = ? AND status = 'leased' AND leased_by = ?;

-- name: AckRunQueueEntry :one
-- Mark a leased entry as successfully completed (terminal).
-- Removes it from the queue (acked is terminal, no longer needed).
-- Scoped to the current lease owner (found by the fifth @oh review pass over
-- #337-349): without this, an executor that lost its lease to a
-- CleanupExpiredLeases recovery (a rare residual now that executeEntry
-- renews its lease for the duration of a real turn, but not impossible
-- under a pathological scheduling stall) could Ack a row a DIFFERENT,
-- currently-live executor now owns, deleting work out from under it.
DELETE FROM session_run_queue
WHERE id = ? AND status = 'leased' AND leased_by = ?
RETURNING id;

-- name: NackRunQueueEntry :one
-- Release a leased entry back to pending state (non-terminal failure, retry later).
-- Increments attempts but does NOT set terminal_failure.
-- Scoped to the current lease owner, same as AckRunQueueEntry.
UPDATE session_run_queue
SET status = 'pending',
    leased_by = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    last_error = ?,
    attempts = attempts + 1,
    updated_at = ?
WHERE id = ? AND status = 'leased' AND leased_by = ?
RETURNING *;

-- name: NackRunQueueEntryNoAttemptPenalty :one
-- Release a leased entry back to pending state without counting it as an
-- attempt. Used specifically for session.SessionLockBusyError: another live
-- process legitimately holding the OS session lock is routine, expected
-- contention, not a failure of the call itself, and must never count toward
-- RunQueueMaxAttempts. Without this, the durable queue would delete
-- accepted user work after nothing more than a few turns of ordinary lock
-- contention. Also used for the ErrCallQueuedNotExecuted backoff path and
-- for releasing a mismatched attempts-exhausted lease unharmed.
-- Scoped to the current lease owner, same as AckRunQueueEntry.
UPDATE session_run_queue
SET status = 'pending',
    leased_by = NULL,
    leased_at = NULL,
    lease_expires_at = NULL,
    last_error = ?,
    updated_at = ?
WHERE id = ? AND status = 'leased' AND leased_by = ?
RETURNING *;

-- name: TerminalFailRunQueueEntry :one
-- Mark a leased entry as terminal failure (no retry, even if attempts < max).
-- Used for ErrCallAlreadyAttempted-type errors where retry would cause duplicates.
-- Scoped to the current lease owner, same as AckRunQueueEntry.
DELETE FROM session_run_queue
WHERE id = ? AND status = 'leased' AND leased_by = ?
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