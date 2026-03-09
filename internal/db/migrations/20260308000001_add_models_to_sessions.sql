-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN large_model_provider TEXT;
ALTER TABLE sessions ADD COLUMN large_model_id TEXT;
ALTER TABLE sessions ADD COLUMN small_model_provider TEXT;
ALTER TABLE sessions ADD COLUMN small_model_id TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN large_model_provider;
ALTER TABLE sessions DROP COLUMN large_model_id;
ALTER TABLE sessions DROP COLUMN small_model_provider;
ALTER TABLE sessions DROP COLUMN small_model_id;
-- +goose StatementEnd
