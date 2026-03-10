-- +goose Up
ALTER TABLE messages ADD COLUMN pinned INTEGER DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN pinned;
