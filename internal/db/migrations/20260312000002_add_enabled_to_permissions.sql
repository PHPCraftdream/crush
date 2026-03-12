-- +goose Up
ALTER TABLE session_permissions ADD COLUMN enabled INTEGER DEFAULT 1 NOT NULL;

-- +goose Down
ALTER TABLE session_permissions DROP COLUMN enabled;
