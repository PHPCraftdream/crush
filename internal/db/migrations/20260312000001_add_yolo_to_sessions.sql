-- +goose Up
ALTER TABLE sessions ADD COLUMN yolo_enabled INTEGER DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN yolo_enabled;
