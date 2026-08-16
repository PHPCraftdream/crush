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

-- name: UpdateMessageIfNotTerminal :execrows
-- P0-4 fix: Only update if the message does NOT already have a non-partial finish.
-- This prevents a hung checkpoint from overwriting a terminal finish after unblocking.
-- A checkpoint writes with Partial=true (finished_at NULL), so this condition rejects
-- the update if finished_at is already non-NULL (real terminal finish).
-- Returns number of rows affected (0 = skipped because already terminal, 1 = updated).
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?
  AND finished_at IS NULL;

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
--
-- NOTE: plain OFFSET is still vulnerable to a DIFFERENT instability than the
-- tie-breaker bug above: concurrent inserts at the head of the DESC order (new
-- messages from a still-running delegation) shift what a given numeric offset
-- points at between two separate calls of this query. This query remains here
-- as the plain, unprotected primitive; message.Service.ListPaginatedSnapshot
-- is the race-free caller-facing wrapper read_delegation_transcript.go
-- actually uses, built from GetTranscriptWindowCursor (pins one consistent
-- snapshot boundary) plus the keyset pair ListMessagesBySessionOlderThan-
-- CreatedAt / ListMessagesBySessionAtCreatedAt (fetches the window strictly
-- at-or-before that boundary, immune to head insertions by construction).
ORDER BY created_at DESC, rowid DESC
LIMIT ? OFFSET ?;

-- name: CountMessagesBySession :one
SELECT COUNT(*)
FROM messages
WHERE session_id = ?;

-- name: GetTranscriptWindowCursor :one
-- Returns, in a SINGLE round trip, the (created_at, rowid) of the row at
-- `offset` positions back from the newest message in the session, together
-- with the session's total message count as of that same statement
-- execution (SQLite evaluates COUNT(*) OVER() over the full WHERE-matched row
-- set before LIMIT/OFFSET are applied, in the same query plan as the LIMIT 1
-- OFFSET ? row selection - both numbers come from one consistent snapshot).
--
-- This is the fix for the cross-statement race in read_delegation_transcript.go:
-- previously Count() and ListPaginated() were two independent queries, so a
-- message inserted between them (the normal case while observing a live
-- sub-agent) could make the total and the offset-derived window disagree.
-- message.Service.ListPaginatedSnapshot calls this query FIRST to pin down a
-- single high-water-mark snapshot (the boundary row's created_at/row_id, plus
-- total), then fetches the window strictly at-or-before that boundary via
-- ListMessagesBySessionOlderThanCreatedAt + ListMessagesBySessionAtCreatedAt.
-- Any message inserted after this query executes is newer than the pinned
-- boundary and is therefore correctly excluded from both the "total" figure
-- and the follow-up window fetch - the read as a whole reflects one
-- consistent point in time rather than drifting across separate round trips.
--
-- created_at/row_id are NULL when offset falls at or past the end of the
-- session's messages (i.e. there is no row at that position): callers must
-- treat NULL as "start of history", matching offset's existing clamp-to-empty
-- behavior in clampTranscriptWindow.
--
-- row_id is exposed via CAST(rowid AS INTEGER): sqlc's SQLite catalog has no
-- notion of SQLite's implicit rowid column outside ORDER BY (confirmed by
-- hand: any bare `rowid` reference in a WHERE clause or plain SELECT-list
-- position fails static analysis with "column \"rowid\" does not exist",
-- while the identical reference wrapped in CAST(... AS INTEGER) in the
-- SELECT list is accepted) - see ListMessagesBySessionOlderThanCreatedAt's
-- comment for why the keyset filter itself therefore avoids rowid in a WHERE
-- clause entirely rather than fighting this further.
SELECT
    created_at,
    CAST(rowid AS INTEGER) AS row_id,
    COUNT(*) OVER () AS total_count
FROM messages
WHERE session_id = ?
ORDER BY created_at DESC, rowid DESC
LIMIT 1 OFFSET ?;

-- name: ListMessagesBySessionOlderThanCreatedAt :many
-- First half of the keyset pagination pair used by
-- read_delegation_transcript.go: returns messages strictly OLDER (by
-- created_at second) than the cursor boundary, deterministically ordered.
-- Combined in Go with ListMessagesBySessionAtCreatedAt (which handles the
-- boundary second's exact-tie rows via the rowid tiebreaker) to reproduce the
-- same (created_at, rowid) keyset semantics as a single
-- `(created_at, rowid) < (?, ?)` comparison would, without needing rowid in a
-- WHERE clause - see GetTranscriptWindowCursor's comment for why that's
-- avoided. Because the boundary is a concrete value pinned by
-- GetTranscriptWindowCursor at the start of the read (not "the Nth row from
-- the current end"), inserting new messages at the head between calls can
-- never shift which rows this query returns: the boundary itself doesn't move.
SELECT *
FROM messages
WHERE session_id = ?
  AND created_at < ?
ORDER BY created_at DESC, rowid DESC
LIMIT ?;

-- name: ListMessagesBySessionAtCreatedAt :many
-- Second half of the keyset pagination pair (see
-- ListMessagesBySessionOlderThanCreatedAt): returns every message in the
-- session sharing the exact boundary second, WITH its rowid (via
-- CAST(rowid AS INTEGER), the one position sqlc's SQLite catalog accepts a
-- bare rowid reference in - see GetTranscriptWindowCursor's comment), so the
-- Go caller can apply the `rowid < boundary_row_id` half of the tiebreaker
-- itself and merge the result with the older-seconds query above into one
-- deterministic (created_at DESC, rowid DESC) window.
SELECT
    sqlc.embed(messages),
    CAST(rowid AS INTEGER) AS row_id
FROM messages
WHERE session_id = ?
  AND created_at = ?
ORDER BY rowid DESC;

-- name: UpdateMessageUsage :exec
-- Per-message token usage and prompt-cache accounting (task #469).
--
-- Written by a separate statement rather than folded into UpdateMessage on
-- purpose: the agent appends the Finish part BEFORE the step's usage is
-- resolved (internal/agent/agent.go - AddFinish precedes fallbackStepUsage),
-- so there is no single moment where both are in hand. Keeping usage separate
-- avoids reordering that finish chain.
--
-- All placeholders are bare `?` in binding order. Do NOT mix bare `?` with
-- numbered/named args here: SQLite numbers a bare `?` from the highest
-- explicit index already used, which silently shifts positional binding.
UPDATE messages
SET
    input_tokens = ?,
    output_tokens = ?,
    reasoning_tokens = ?,
    cache_creation_tokens = ?,
    cache_read_tokens = ?,
    total_tokens = ?,
    cost_usd = ?,
    usage_provider = ?,
    usage_model = ?,
    cache_support = ?,
    usage_estimated = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: SumMessageUsageBySession :many
-- Cache/token/cost aggregate for one session, grouped by the model that
-- actually produced each message (a session can switch models mid-run, and
-- averaging their cache behaviour into one number describes neither).
--
-- recorded/estimated are returned so a caller can state its coverage rather
-- than implying a total was computed over everything: rows written before this
-- feature existed have NULL usage and are excluded by the WHERE clause.
--
-- Every SUM is wrapped in CAST: without it sqlc infers interface{} for an
-- aggregate over a nullable column and the generated struct is unusable.
SELECT
    COALESCE(usage_provider, '') AS provider,
    COALESCE(usage_model, '') AS model,
    COALESCE(cache_support, '') AS cache_support,
    CAST(COUNT(*) AS INTEGER) AS recorded,
    CAST(COALESCE(SUM(usage_estimated), 0) AS INTEGER) AS estimated,
    CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER) AS input_tokens,
    CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER) AS output_tokens,
    CAST(COALESCE(SUM(reasoning_tokens), 0) AS INTEGER) AS reasoning_tokens,
    CAST(COALESCE(SUM(cache_creation_tokens), 0) AS INTEGER) AS cache_creation_tokens,
    CAST(COALESCE(SUM(cache_read_tokens), 0) AS INTEGER) AS cache_read_tokens,
    CAST(COALESCE(SUM(total_tokens), 0) AS INTEGER) AS total_tokens,
    CAST(COALESCE(SUM(cost_usd), 0) AS REAL) AS cost_usd
FROM messages
WHERE session_id = ? AND total_tokens IS NOT NULL
GROUP BY usage_provider, usage_model, cache_support;

-- name: CountMessagesMissingUsage :one
-- Assistant rows in a session that carry no usage at all. Reported alongside
-- any aggregate so "12% cache hit" is never presented without saying how many
-- messages it was computed over.
SELECT COUNT(*) FROM messages
WHERE session_id = ? AND role = 'assistant' AND total_tokens IS NULL;
