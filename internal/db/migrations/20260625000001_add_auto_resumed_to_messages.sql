-- +goose Up
ALTER TABLE messages ADD COLUMN auto_resumed INTEGER DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN auto_resumed;
