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

-- name: ListSessions :many
SELECT *
FROM sessions
WHERE parent_session_id is NULL
ORDER BY updated_at DESC;

-- name: UpdateSession :one
UPDATE sessions
SET
    title = ?,
    prompt_tokens = ?,
    completion_tokens = ?,
    summary_message_id = ?,
    cost = ?,
    todos = ?
WHERE id = ?
RETURNING *;

-- name: UpdateSessionTitleAndUsage :exec
UPDATE sessions
SET
    title = ?,
    prompt_tokens = prompt_tokens + ?,
    completion_tokens = completion_tokens + ?,
    cost = cost + ?
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
