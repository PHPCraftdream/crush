-- +goose Up
-- +goose StatementBegin
-- session_run_queue is a durable per-session run queue for orphaned/detached calls.
-- It ensures that once a call is accepted (status=queued returned to caller), it has
-- a durable row that guarantees eventual execution by some process, independent of
-- which specific request/turn or process originated it.
--
-- This is NOT the batch CLI queue (queue_tasks) — that serves independent CLI
-- invocations. This queue serves orphaned recovery within a single session's
-- lifecycle across process boundaries.
--
-- States: pending → leased → acked (acked is terminal, removed by ack)
-- pending: call is waiting for a runner
-- leased: a pump instance has claimed it (with TTL)
--
-- Idempotency: the id field is generated once at enqueue time and used for all
-- subsequent retry/pickup operations, protecting against duplicate execution
-- even across process boundaries (fixes task #339 regression).
CREATE TABLE IF NOT EXISTS session_run_queue (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,

    -- Serialized SessionAgentCall (all fields needed to recreate the call)
    call_data TEXT NOT NULL,

    -- Queue state machine
    status TEXT NOT NULL CHECK(status IN ('pending', 'leased', 'acked')),

    -- Lease tracking (for pending→leased transition)
    leased_by TEXT,
    leased_at INTEGER,
    lease_expires_at INTEGER,

    -- Retry tracking
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,

    -- Terminal failure flag: when true, the call will NOT be retried even if
    -- attempts < max_attempts. Used for ErrCallAlreadyAttempted-type errors
    -- where retry would cause duplicates (task #339 fix).
    terminal_failure INTEGER NOT NULL DEFAULT 0,

    -- Timestamps
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Index for efficient pending/leased scanning
CREATE INDEX IF NOT EXISTS idx_session_run_queue_status ON session_run_queue (status, created_at);
-- Index for session-specific queries
CREATE INDEX IF NOT EXISTS idx_session_run_queue_session_id ON session_run_queue (session_id);
-- Index for lease expiration scanning (find stale leased records)
CREATE INDEX IF NOT EXISTS idx_session_run_queue_lease_expiry ON session_run_queue (lease_expires_at) WHERE status = 'leased';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_session_run_queue_lease_expiry;
DROP INDEX IF EXISTS idx_session_run_queue_session_id;
DROP INDEX IF EXISTS idx_session_run_queue_status;
DROP TABLE IF EXISTS session_run_queue;
-- +goose StatementEnd