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
UPDATE sessions
SET
    large_model_provider = ?,
    large_model_id = ?,
    small_model_provider = ?,
    small_model_id = ?,
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

-- name: UpdateSession :one
-- Overwrites title/prompt_tokens/completion_tokens/summary/todos but NOT
-- cost. Cost is mutated only via IncrementSessionCost so concurrent
-- sub-agent goroutines (and parallel crush processes that ever share a
-- session) cannot lose accrued cost via read-modify-write.
UPDATE sessions
SET
    title = ?,
    prompt_tokens = ?,
    completion_tokens = ?,
    summary_message_id = ?,
    todos = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?
RETURNING *;

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

-- name: UpdateSessionTitleAndUsage :exec
UPDATE sessions
SET
    title = ?,
    prompt_tokens = prompt_tokens + ?,
    completion_tokens = completion_tokens + ?,
    cost = cost + ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: RenameSession :exec
UPDATE sessions
SET
    title = ?
WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions
WHERE id = ?;

-- name: SetSessionYolo :exec
UPDATE sessions
SET yolo_enabled = ?,
    updated_at = strftime('%s', 'now')
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
