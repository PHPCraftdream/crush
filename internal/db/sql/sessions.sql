-- name: CreateSession :one
INSERT INTO sessions (
    id,
    parent_session_id,
    title,
    message_count,
    prompt_tokens,
    completion_tokens,
    cost,
    summary_message_id,
    updated_at,
    created_at,
    large_model_provider,
    large_model_id,
    small_model_provider,
    small_model_id,
    yolo_enabled
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    null,
    strftime('%s', 'now'),
    strftime('%s', 'now'),
    ?,
    ?,
    ?,
    ?,
    0
) RETURNING *;

-- name: UpdateSessionModels :exec
-- Partial update: a NULL arg for a slot's provider/id pair leaves that slot
-- untouched (COALESCE falls back to the current column value); a non-NULL
-- arg (including an explicit empty string) overwrites it. This lets callers
-- distinguish "don't touch this slot" from "clear this slot back to
-- inheriting the folder/system default" (large_model_id = '' is the existing
-- "no override" convention the app layer already reads via != "").
UPDATE sessions
SET
    large_model_provider = COALESCE(sqlc.narg('large_model_provider'), large_model_provider),
    large_model_id = COALESCE(sqlc.narg('large_model_id'), large_model_id),
    small_model_provider = COALESCE(sqlc.narg('small_model_provider'), small_model_provider),
    small_model_id = COALESCE(sqlc.narg('small_model_id'), small_model_id),
    updated_at = strftime('%s', 'now')
WHERE id = sqlc.arg('id');

-- name: UpdateSessionWorkerReviewerModels :exec
-- Same partial-update semantics as UpdateSessionModels: a NULL arg leaves
-- that slot untouched, a non-NULL arg (including an explicit empty string)
-- overwrites it.
UPDATE sessions
SET
    worker_model_provider = COALESCE(sqlc.narg('worker_model_provider'), worker_model_provider),
    worker_model_id = COALESCE(sqlc.narg('worker_model_id'), worker_model_id),
    reviewer_model_provider = COALESCE(sqlc.narg('reviewer_model_provider'), reviewer_model_provider),
    reviewer_model_id = COALESCE(sqlc.narg('reviewer_model_id'), reviewer_model_id),
    updated_at = strftime('%s', 'now')
WHERE id = sqlc.arg('id');

-- name: UpdateSessionWorkerReviewerReasoningEffort :exec
UPDATE sessions
SET
    worker_model_reasoning_effort = ?,
    reviewer_model_reasoning_effort = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: GetSessionByID :one
SELECT *
FROM sessions
WHERE id = ? LIMIT 1;

-- name: GetLastSession :one
SELECT *
FROM sessions
ORDER BY updated_at DESC
LIMIT 1;

-- name: ListSessions :many
SELECT *
FROM sessions
WHERE parent_session_id is NULL
ORDER BY updated_at DESC;

-- name: ListAllSessions :many
-- Returns every session including children (no parent_session_id filter).
-- Used by sessions gc to enumerate all sessions for garbage collection.
SELECT *
FROM sessions
ORDER BY updated_at DESC;

-- name: ListSubSessions :many
-- Returns every session whose parent_session_id matches the argument,
-- ordered oldest-first so callers reconstructing a fan-out get the
-- sub-agent results in dispatch order.
SELECT *
FROM sessions
WHERE parent_session_id = ?
ORDER BY created_at ASC;

-- name: IncrementSessionCost :one
-- Atomic additive update for session cost. Safe under fan-out (multiple
-- sub-agent goroutines finishing concurrently and each charging the
-- parent) and across processes (orchestrator with parallel crush runs).
-- Returns the updated row so the caller can refresh its snapshot.
UPDATE sessions
SET
    cost = cost + ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?
RETURNING *;

-- name: RenameSession :exec
UPDATE sessions
SET
    title = ?
WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions
WHERE id = ?;

-- name: UpdateSessionSystemPrompt :exec
UPDATE sessions
SET
    system_prompt = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: UpdateSessionReasoningEffort :exec
UPDATE sessions
SET
    large_model_reasoning_effort = ?,
    small_model_reasoning_effort = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: GetSessionCostAccounting :one
-- Returns the child's current cost and the amount already charged to the
-- parent (parent_cost_accounted). Used by TransferChildCostToParent inside
-- a transaction so delta = cost - accounted is computed from a single
-- consistent read within that transaction.
SELECT cost, parent_cost_accounted
FROM sessions
WHERE id = ? LIMIT 1;

-- name: SetParentCostAccounted :exec
-- Marks the child's full current cost as charged to the parent, so the
-- next TransferChildCostToParent call charges only new cost accrued above
-- this point. Run inside the same transaction as the parent's
-- IncrementSessionCost so a crash between the two cannot leave the parent
-- charged but the child's accounting lagging (or vice versa).
UPDATE sessions
SET
    parent_cost_accounted = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;
