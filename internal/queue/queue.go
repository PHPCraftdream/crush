// Package queue provides persistent task queue CRUD for batched crush run
// invocations. The backing store is a SQLite table (queue_tasks) created by
// migration 20260520000002.
package queue

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TaskStatus represents the lifecycle state of a queue task.
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusDone      TaskStatus = "done"
	StatusFailed    TaskStatus = "failed"
	StatusCancelled TaskStatus = "cancelled"
)

// Task is a row from the queue_tasks table.
type Task struct {
	ID         string
	SessionID  string
	Prompt     string
	Role       string
	MaxCost    float64
	MaxTokens  int64
	TimeoutSec int64
	Status     TaskStatus
	Cost       float64
	Tokens     int64
	ExitReason string
	CreatedAt  int64
	StartedAt  sql.NullInt64
	FinishedAt sql.NullInt64
}

// Service provides CRUD for the queue_tasks table.
type Service struct {
	db *sql.DB
}

// NewService creates a queue service backed by the given database.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// Add inserts a new pending task and returns its generated ID.
func (s *Service) Add(ctx context.Context, sessionID, prompt, role string, maxCost float64, maxTokens, timeoutSec int64) (string, error) {
	id := fmt.Sprintf("q-%d-%s", time.Now().Unix(), uuid.New().String()[:8])
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO queue_tasks (id, session_id, prompt, role, max_cost, max_tokens, timeout_sec, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		id, sql.NullString{String: sessionID, Valid: sessionID != ""},
		prompt, sql.NullString{String: role, Valid: role != ""},
		maxCost, maxTokens, timeoutSec, now,
	)
	if err != nil {
		return "", fmt.Errorf("queue add: %w", err)
	}
	return id, nil
}

// List returns tasks filtered by status. Empty status means all.
func (s *Service) List(ctx context.Context, status TaskStatus) ([]Task, error) {
	query := "SELECT id, COALESCE(session_id,''), prompt, COALESCE(role,''), COALESCE(max_cost,0), COALESCE(max_tokens,0), COALESCE(timeout_sec,0), status, cost, tokens, COALESCE(exit_reason,''), created_at, started_at, finished_at FROM queue_tasks"
	var args []any
	if status != "" {
		query += " WHERE status = ?"
		args = append(args, string(status))
	}
	query += " ORDER BY created_at ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Prompt, &t.Role,
			&t.MaxCost, &t.MaxTokens, &t.TimeoutSec, &t.Status,
			&t.Cost, &t.Tokens, &t.ExitReason,
			&t.CreatedAt, &t.StartedAt, &t.FinishedAt,
		); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Get returns a single task by ID.
func (s *Service) Get(ctx context.Context, id string) (Task, error) {
	var t Task
	err := s.db.QueryRowContext(ctx,
		"SELECT id, COALESCE(session_id,''), prompt, COALESCE(role,''), COALESCE(max_cost,0), COALESCE(max_tokens,0), COALESCE(timeout_sec,0), status, cost, tokens, COALESCE(exit_reason,''), created_at, started_at, finished_at FROM queue_tasks WHERE id = ?",
		id,
	).Scan(&t.ID, &t.SessionID, &t.Prompt, &t.Role,
		&t.MaxCost, &t.MaxTokens, &t.TimeoutSec, &t.Status,
		&t.Cost, &t.Tokens, &t.ExitReason,
		&t.CreatedAt, &t.StartedAt, &t.FinishedAt,
	)
	return t, err
}

// ClaimPending atomically picks up to n pending tasks and sets them to
// running. Returns the claimed tasks.
func (s *Service) ClaimPending(ctx context.Context, n int) ([]Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		"SELECT id, COALESCE(session_id,''), prompt, COALESCE(role,''), COALESCE(max_cost,0), COALESCE(max_tokens,0), COALESCE(timeout_sec,0), status, cost, tokens, COALESCE(exit_reason,''), created_at, started_at, finished_at FROM queue_tasks WHERE status = 'pending' ORDER BY created_at ASC LIMIT ?", n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Prompt, &t.Role,
			&t.MaxCost, &t.MaxTokens, &t.TimeoutSec, &t.Status,
			&t.Cost, &t.Tokens, &t.ExitReason,
			&t.CreatedAt, &t.StartedAt, &t.FinishedAt,
		); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Release the read cursor before issuing writes on the same tx (SQLite
	// dislikes an open cursor during writes); the deferred Close is then a
	// harmless no-op.
	rows.Close()

	now := time.Now().Unix()
	for _, t := range tasks {
		if _, err := tx.ExecContext(ctx,
			"UPDATE queue_tasks SET status = 'running', started_at = ? WHERE id = ?",
			now, t.ID,
		); err != nil {
			return nil, err
		}
	}
	return tasks, tx.Commit()
}

// UpdateStatus sets the task's final state and metrics.
func (s *Service) UpdateStatus(ctx context.Context, id string, status TaskStatus, cost float64, tokens int64, exitReason string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		"UPDATE queue_tasks SET status = ?, cost = ?, tokens = ?, exit_reason = ?, finished_at = ? WHERE id = ?",
		string(status), cost, tokens, exitReason, now, id,
	)
	return err
}

// ReclaimRunning resets every task stuck in 'running' back to 'pending' so a
// future `queue run` can pick it up again, clearing the run-scoped fields
// (cost/tokens/exit_reason/started_at/finished_at) left over from the
// abandoned attempt.
//
// Callers MUST call this only while holding the process-exclusive queue.lock
// OS lock (see acquireSpawnLock in internal/cmd/queue.go) — winning that lock
// is authoritative proof no other `queue run` is alive, because a live
// runner keeps the OS lock held for its entire run (flock/LockFileEx
// auto-releases only on process death, mirroring internal/session/lock.go's
// SessionLock). Given that invariant, EVERY row still marked 'running' at
// this point necessarily belongs to a previous runner that died or was
// killed before it could write a final status; there is no live owner it
// could ever steal from.
//
// This deliberately does NOT use a per-task lease, heartbeat, runner ID, or
// any mtime/PID-based liveness threshold. Task #269 (see
// internal/session/lock.go's package doc) found that reclaiming a lock by
// "PID looks dead" or "mtime looks stale" can steal ownership from a holder
// that is merely busy on one long tool call (up to ~45 minutes is normal),
// producing two simultaneous owners. That failure mode requires multiple
// candidate owners racing a time/liveness guess. The queue has none: only
// one process can ever hold queue.lock at a time, so there is nothing for a
// threshold to disambiguate — the OS lock itself is the exact, zero-latency
// answer "is a previous runner still alive", with no window in which a
// healthy runner can be mistaken for a dead one.
func (s *Service) ReclaimRunning(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE queue_tasks
		    SET status = 'pending', cost = 0, tokens = 0, exit_reason = '',
		        started_at = NULL, finished_at = NULL
		  WHERE status = 'running'`,
	)
	if err != nil {
		return 0, fmt.Errorf("reclaim running tasks: %w", err)
	}
	return res.RowsAffected()
}

// Remove deletes a task by ID (only if in a terminal state).
func (s *Service) Remove(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM queue_tasks WHERE id = ? AND status IN ('pending', 'done', 'failed', 'cancelled')",
		id,
	)
	return err
}

// Clear removes tasks matching the given statuses. Empty means all terminal states.
func (s *Service) Clear(ctx context.Context, statuses ...TaskStatus) error {
	if len(statuses) == 0 {
		statuses = []TaskStatus{StatusPending, StatusDone, StatusFailed, StatusCancelled}
	}
	ph := ""
	args := make([]any, len(statuses))
	for i, s := range statuses {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args[i] = string(s)
	}
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM queue_tasks WHERE status IN ("+ph+")",
		args...,
	)
	return err
}
