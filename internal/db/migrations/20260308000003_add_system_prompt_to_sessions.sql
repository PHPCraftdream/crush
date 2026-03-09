-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN system_prompt TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN system_prompt;
-- +goose StatementEnd
