-- +goose Up
-- +goose StatementBegin
-- pending_injects is a cross-process signal/queue table for
-- `crush sessions inject`: one process (the CLI) writes a row here to ask
-- another already-running `crush run` process to splice a message into the
-- session it is actively executing. The message row itself is created in
-- `messages` at inject time (for immediate web-UI visibility, mirroring
-- handleInjectMessage); this table only references it by message_id so the
-- source of truth for what to merge is the messages table. content is kept
-- for debugging/logging only.
--
-- interrupt = 0 rows are drained + merged on the next PrepareStep by the
-- running agent (delete-after-read). interrupt = 1 rows are owned by the
-- interrupt ticker (a separate feature) which must consume them before the
-- next PrepareStep; PrepareStep only reports their presence, it does not
-- delete them.
CREATE TABLE IF NOT EXISTS pending_injects (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    interrupt INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_pending_injects_session_id ON pending_injects (session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_pending_injects_session_id;
DROP TABLE IF EXISTS pending_injects;
-- +goose StatementEnd
