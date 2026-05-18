-- Fork patch: batch 30 — runaway protection.
-- Add cancel_requested flag so a foreign process (orchestrator) can signal
-- a running `crush run` to stop gracefully within one step.
--
-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN cancel_requested;
-- +goose StatementEnd
