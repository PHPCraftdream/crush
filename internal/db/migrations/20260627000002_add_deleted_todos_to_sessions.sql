-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN deleted_todos TEXT NOT NULL DEFAULT '[]';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN deleted_todos;
-- +goose StatementEnd
