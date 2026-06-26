-- +goose Up
ALTER TABLE messages ADD COLUMN background_job_notice INTEGER DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN background_job_notice;
