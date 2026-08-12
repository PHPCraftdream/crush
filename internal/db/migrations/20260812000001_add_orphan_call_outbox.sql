-- +goose Up
-- +goose StatementBegin
-- orphan_call_outbox is a durable fallback table for calls that were accepted
-- but could not be enqueued to the main run queue (session_run_queue).
--
-- This table provides a minimal durability guarantee: when the main run queue
-- write fails (e.g., due to transient DB issues), we still have a durable
-- record of the accepted call that can be recovered later.
--
-- States: pending -> processing -> done (terminal) or failed (terminal)
-- pending: call is waiting to be retried (moved to main run queue)
-- processing: call is currently being moved to main run queue
-- failed: all retry attempts exhausted, call is terminally lost
-- done: successfully moved to main run queue
--
-- Recovery: the pump can periodically scan this table for pending entries
-- and attempt to move them to the main run queue. If that succeeds, mark as done.
-- If it fails after bounded retries, mark as failed.
--
-- This is NOT a general-purpose outbox for all writes — it's a narrow
-- fallback specifically for orphan recovery when the main run queue is
-- unavailable (P0-3 fix).
CREATE TABLE IF NOT EXISTS orphan_call_outbox (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,

    -- Serialized SessionAgentCall (all fields needed to recreate the call)
    call_data TEXT NOT NULL,

    -- Outbox state machine
    status TEXT NOT NULL CHECK(status IN ('pending', 'processing', 'done', 'failed')),

    -- Retry tracking (bounded retries to avoid infinite loops)
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    last_error TEXT,

    -- Timestamps
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Index for efficient pending scanning (for pump recovery)
CREATE INDEX IF NOT EXISTS idx_orphan_call_outbox_status ON orphan_call_outbox (status, created_at);
-- Index for session-specific queries
CREATE INDEX IF NOT EXISTS idx_orphan_call_outbox_session_id ON orphan_call_outbox (session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_orphan_call_outbox_session_id;
DROP INDEX IF EXISTS idx_orphan_call_outbox_status;
DROP TABLE IF EXISTS orphan_call_outbox;
-- +goose StatementEnd