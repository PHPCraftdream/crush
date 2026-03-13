-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN large_model_reasoning_effort TEXT DEFAULT 'medium';
ALTER TABLE sessions ADD COLUMN small_model_reasoning_effort TEXT DEFAULT 'medium';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN large_model_reasoning_effort;
ALTER TABLE sessions DROP COLUMN small_model_reasoning_effort;
-- +goose StatementEnd
