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
