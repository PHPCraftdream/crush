-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN worker_model_provider TEXT;
ALTER TABLE sessions ADD COLUMN worker_model_id TEXT;
ALTER TABLE sessions ADD COLUMN worker_model_reasoning_effort TEXT;
ALTER TABLE sessions ADD COLUMN reviewer_model_provider TEXT;
ALTER TABLE sessions ADD COLUMN reviewer_model_id TEXT;
ALTER TABLE sessions ADD COLUMN reviewer_model_reasoning_effort TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN worker_model_provider;
ALTER TABLE sessions DROP COLUMN worker_model_id;
ALTER TABLE sessions DROP COLUMN worker_model_reasoning_effort;
ALTER TABLE sessions DROP COLUMN reviewer_model_provider;
ALTER TABLE sessions DROP COLUMN reviewer_model_id;
ALTER TABLE sessions DROP COLUMN reviewer_model_reasoning_effort;
-- +goose StatementEnd
