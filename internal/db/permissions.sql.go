package db

import "context"

// SessionPermission is a persistent per-session tool permission stored in the DB.
type SessionPermission struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	ToolName  string `json:"tool_name"`
	Action    string `json:"action"`
	Path      string `json:"path"`
	CreatedAt int64  `json:"created_at"`
}

const createSessionPermission = `INSERT INTO session_permissions (id, session_id, tool_name, action, path)
VALUES (?, ?, ?, ?, ?)`

type CreateSessionPermissionParams struct {
	ID        string
	SessionID string
	ToolName  string
	Action    string
	Path      string
}

func (q *Queries) CreateSessionPermission(ctx context.Context, arg CreateSessionPermissionParams) error {
	_, err := q.exec(ctx, q.createSessionPermissionStmt, createSessionPermission,
		arg.ID, arg.SessionID, arg.ToolName, arg.Action, arg.Path)
	return err
}

const listAllSessionPermissions = `SELECT id, session_id, tool_name, action, path, created_at FROM session_permissions`

func (q *Queries) ListAllSessionPermissions(ctx context.Context) ([]SessionPermission, error) {
	rows, err := q.query(ctx, q.listAllSessionPermissionsStmt, listAllSessionPermissions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []SessionPermission
	for rows.Next() {
		var i SessionPermission
		if err := rows.Scan(&i.ID, &i.SessionID, &i.ToolName, &i.Action, &i.Path, &i.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}
