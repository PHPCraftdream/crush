-- Fork patch: batch 30 — queue system.
-- Persistent task queue for batched `crush run` invocations.
--
-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS queue_tasks (
    id          TEXT PRIMARY KEY,
    session_id  TEXT,
    prompt      TEXT NOT NULL,
    role        TEXT,
    max_cost    REAL,
    max_tokens  INTEGER,
    timeout_sec INTEGER,
    status      TEXT NOT NULL CHECK(status IN ('pending','running','done','failed','cancelled')),
    cost        REAL    DEFAULT 0,
    tokens      INTEGER DEFAULT 0,
    exit_reason TEXT,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_queue_status ON queue_tasks(status, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS queue_tasks;
-- +goose StatementEnd
