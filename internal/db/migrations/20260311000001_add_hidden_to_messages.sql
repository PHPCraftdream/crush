-- +goose Up
ALTER TABLE messages ADD COLUMN hidden INTEGER DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN hidden;
