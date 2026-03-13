-- +goose Up
-- +goose StatementBegin
ALTER TABLE messages ADD COLUMN reasoning_effort TEXT DEFAULT 'medium';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE messages DROP COLUMN reasoning_effort;
-- +goose StatementEnd
