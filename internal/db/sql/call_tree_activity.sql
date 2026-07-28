-- call_tree_activity.sql: freshest message activity across a session's whole
-- descendant call tree (root + every sub-agent session reachable via
-- parent_session_id), in ONE recursive-CTE query per root.
--
-- Bounded vs. unbounded:
--   * DEPTH is capped: the recursive member carries `tree.depth < 511`, so a
--     pathological deep chain (or an accidental parent/child cycle) can never
--     recurse indefinitely (see TestGetCallTreeActivity_DepthGuard).
--   * WIDTH (fan-out) is NOT bounded by row count. A single root with many
--     direct children produces that many rows in the CTE regardless of depth.
--     This is a deliberate, documented choice, not an oversight: SQLite's
--     recursive CTE cannot bound the TOTAL number of materialized rows -- the
--     recursive member only sees the PREVIOUS iteration's working table, never
--     the accumulated set, so a running "rows visited" counter is not
--     expressible in the recursion, and SQLite does not support LIMIT inside
--     the recursive member. Bounding fan-out would therefore require either
--     abandoning this SQL CTE (the SQL form replaced the Go BFS in task #104)
--     or reverting to a Go-side BFS, neither of which is warranted: in
--     practice a parent session spawns at most a handful of concurrent
--     sub-agent delegations, so unbounded fan-out is not a real performance
--     risk. The single-root form also ends in LIMIT 1 and the batch form in a
--     per-root ROW_NUMBER()=1 filter, so only one row per root ever leaves
--     the query.
-- name: GetCallTreeActivity :one
WITH RECURSIVE tree AS (
    SELECT s0.id AS session_id, 0 AS depth FROM sessions s0 WHERE s0.id = ?
    UNION ALL
    SELECT s.id AS session_id, tree.depth + 1 AS depth
    FROM sessions s, tree
    WHERE s.parent_session_id = tree.session_id
    AND tree.depth < 511
)
SELECT
    m.session_id AS session_id,
    m.role AS role,
    CAST(MAX(m.created_at, m.updated_at) AS INTEGER) AS latest_unix,
    tree.depth AS depth
FROM tree
JOIN messages m ON m.session_id = tree.session_id
ORDER BY
    latest_unix DESC,
    depth DESC,
    m.session_id ASC
LIMIT 1;

-- name: GetCallTreeActivityBatch :many
WITH RECURSIVE tree AS (
    SELECT s0.id AS root_session_id, s0.id AS session_id, 0 AS depth
    FROM sessions s0
    WHERE s0.id IN (sqlc.slice('root_ids'))
    UNION ALL
    SELECT tree.root_session_id AS root_session_id, s.id AS session_id, tree.depth + 1 AS depth
    FROM sessions s, tree
    WHERE s.parent_session_id = tree.session_id
    AND tree.depth < 511
),
activity AS (
    SELECT
        tree.root_session_id AS root_session_id,
        m.session_id AS session_id,
        m.role AS role,
        CAST(MAX(m.created_at, m.updated_at) AS INTEGER) AS latest_unix,
        tree.depth AS depth
    FROM tree
    JOIN messages m ON m.session_id = tree.session_id
),
ranked AS (
    SELECT
        root_session_id,
        session_id,
        role,
        latest_unix,
        depth,
        ROW_NUMBER() OVER (
            PARTITION BY root_session_id
            ORDER BY latest_unix DESC, depth DESC, session_id ASC
        ) AS rn
    FROM activity
)
SELECT
    root_session_id,
    session_id,
    role,
    latest_unix
FROM ranked
WHERE rn = 1;
