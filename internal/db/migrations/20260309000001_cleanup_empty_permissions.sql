-- +goose Up
-- +goose StatementBegin
DELETE FROM session_permissions WHERE tool_name = '' OR action = '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1; -- irreversible
-- +goose StatementEnd
