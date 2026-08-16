-- Per-message token usage and prompt-cache accounting (task #469).
--
-- Until now token usage existed only as a session-level LAST-SNAPSHOT
-- (sessions.prompt_tokens / completion_tokens, overwritten each turn) plus a
-- running cost total, so there was no way to ask "how well is the prompt cache
-- working" for a message, a model, or a day.
--
-- All columns are NULLABLE with no default on purpose: NULL means "this row
-- predates the feature / usage was never recorded", which must stay
-- distinguishable from a genuine measured zero. Aggregations have to count and
-- report skipped NULL rows rather than treating them as zeros.
--
-- Convention (enforced in internal/agent/cliprovider/usage.go): input_tokens,
-- cache_read_tokens and cache_creation_tokens are DISJOINT; the prompt size is
-- their sum. Do not store a provider's inclusive prompt counter here.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE messages ADD COLUMN input_tokens INTEGER;
ALTER TABLE messages ADD COLUMN output_tokens INTEGER;
ALTER TABLE messages ADD COLUMN reasoning_tokens INTEGER;
ALTER TABLE messages ADD COLUMN cache_creation_tokens INTEGER;
ALTER TABLE messages ADD COLUMN cache_read_tokens INTEGER;
ALTER TABLE messages ADD COLUMN total_tokens INTEGER;
ALTER TABLE messages ADD COLUMN cost_usd REAL;
ALTER TABLE messages ADD COLUMN usage_provider TEXT;
ALTER TABLE messages ADD COLUMN usage_model TEXT;
ALTER TABLE messages ADD COLUMN cache_support TEXT;
ALTER TABLE messages ADD COLUMN usage_estimated INTEGER;

-- Analytics reads are "all recorded usage for a session" and "group by model";
-- a partial index keeps the common case from scanning rows that never got
-- usage (every user message, and every assistant row written before this
-- migration).
CREATE INDEX IF NOT EXISTS idx_messages_usage_session
    ON messages (session_id, usage_model)
    WHERE total_tokens IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_usage_session;
ALTER TABLE messages DROP COLUMN input_tokens;
ALTER TABLE messages DROP COLUMN output_tokens;
ALTER TABLE messages DROP COLUMN reasoning_tokens;
ALTER TABLE messages DROP COLUMN cache_creation_tokens;
ALTER TABLE messages DROP COLUMN cache_read_tokens;
ALTER TABLE messages DROP COLUMN total_tokens;
ALTER TABLE messages DROP COLUMN cost_usd;
ALTER TABLE messages DROP COLUMN usage_provider;
ALTER TABLE messages DROP COLUMN usage_model;
ALTER TABLE messages DROP COLUMN cache_support;
ALTER TABLE messages DROP COLUMN usage_estimated;
-- +goose StatementEnd
