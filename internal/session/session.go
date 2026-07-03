package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
	"github.com/zeebo/xxh3"
)

type TodoStatus string

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusCompleted  TodoStatus = "completed"
)

// HashID returns the XXH3 hash of a session ID (UUID) as a hex string.
func HashID(id string) string {
	h := xxh3.New()
	h.WriteString(id)
	return fmt.Sprintf("%x", h.Sum(nil))
}

type Todo struct {
	Content    string     `json:"content"`
	Status     TodoStatus `json:"status"`
	ActiveForm string     `json:"active_form"`
}

// HasIncompleteTodos returns true if there are any non-completed todos.
func HasIncompleteTodos(todos []Todo) bool {
	for _, todo := range todos {
		if todo.Status != TodoStatusCompleted {
			return true
		}
	}
	return false
}

type Session struct {
	ID               string
	ParentSessionID  string
	Title            string
	MessageCount     int64
	PromptTokens     int64
	CompletionTokens int64
	SummaryMessageID string
	Cost             float64
	Todos            []Todo
	CreatedAt        int64
	UpdatedAt        int64

	LargeModelProvider        string
	LargeModelID              string
	LargeModelReasoningEffort string // "low", "medium", "high", or "max"
	SmallModelProvider        string
	SmallModelID              string
	SmallModelReasoningEffort string // "low", "medium", "high", or "max"

	SystemPrompt    string
	YoloEnabled     bool
	CancelRequested bool // Only populated by ListAll; use IsCancelRequested() for live checks.

	// DeletedTodos holds the Content strings of todos that the operator
	// explicitly removed via the UI. mergeTodos uses this set as a tombstone
	// filter so the model cannot resurrect them during multi-step turns.
	DeletedTodos []string

	// Fork patch (operator UX): persisted from --max-cost / --max-tokens /
	// --timeout at run start so sessions show/locks can display budget.
	EndedReason      string  // "done","canceled","timeout","max_cost","max_tokens","error","crash",""
	BudgetMaxCost    float64 // --max-cost value, 0 if unlimited
	BudgetMaxTokens  int64   // --max-tokens value, 0 if unlimited
	BudgetTimeoutSec int64   // --timeout in seconds, 0 if unlimited

	// Wire-only fields filled by the web server when sending Session over WS;
	// NOT persisted to SQLite. Together they answer "is this session being
	// driven by another live process right now?" so the web UI can render
	// foreign sessions read-only with a "Followed: PID N" banner.
	OwnedExternal bool `json:",omitempty"` // a different live process holds the lock
	OwnedByPID    int  `json:",omitempty"` // PID of the lock holder, 0 if free / stale
}

type Service interface {
	pubsub.Subscriber[Session]
	Create(ctx context.Context, title string) (Session, error)
	// CreateWithID creates a top-level session with a caller-chosen ID. Used
	// by `crush run --session <id>` to make CLI/CI invocations idempotent:
	// the same ID across runs continues the same conversation. Returns an
	// error if a row with that ID already exists (UNIQUE constraint).
	CreateWithID(ctx context.Context, id, title string) (Session, error)
	CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error)
	CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error)
	Get(ctx context.Context, id string) (Session, error)
	GetLast(ctx context.Context) (Session, error)
	List(ctx context.Context) ([]Session, error)
	// ListAll returns every session including children (no parent_session_id
	// filter). Used by sessions gc for garbage collection.
	ListAll(ctx context.Context) ([]Session, error)
	// ListSubSessions returns every session whose parent_session_id
	// equals the argument, ordered oldest-first. Used by the
	// --aggregation=attach path and the reduction-loss warning to
	// gather a parent run's sub-agent fan-out outputs after Run()
	// returns.
	ListSubSessions(ctx context.Context, parentSessionID string) ([]Session, error)
	Save(ctx context.Context, session Session) (Session, error)
	// IncrementCost atomically adds delta to the session's cost via an
	// additive SQL UPDATE. Use this instead of read-modify-write through
	// Save when accruing per-step or per-sub-agent cost: it is race-free
	// under fan-out (multiple sub-agent goroutines completing concurrently
	// and each charging the same parent) and across processes that ever
	// share a session ID. Returns the refreshed session snapshot.
	//
	// Semantics for delta = 0: the implementation short-circuits to a
	// plain Get so callers can use IncrementCost(id, 0) as a "verify the
	// session exists and grab its current snapshot" call without paying
	// the cost of an UPDATE. This preserves the not-found error path for
	// callers like coordinator.updateParentSessionCost where a child
	// with zero accrued cost still wants to fail if the parent went
	// away. Pass a non-zero delta only when you actually want to charge.
	IncrementCost(ctx context.Context, sessionID string, delta float64) (Session, error)
	UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error
	UpdateModels(ctx context.Context, sessionID, largeProvider, largeModel, smallProvider, smallModel string) error
	UpdateReasoningEffort(ctx context.Context, sessionID, largeEffort, smallEffort string) error
	UpdateSystemPrompt(ctx context.Context, sessionID, prompt string) error
	Rename(ctx context.Context, id string, title string) error
	Delete(ctx context.Context, id string) error

	// CancelRequested flag: cross-process cancel signal.
	RequestCancel(ctx context.Context, sessionID string) error
	IsCancelRequested(ctx context.Context, sessionID string) (bool, error)
	ClearCancelRequest(ctx context.Context, sessionID string) error

	// Fork patch: ended_reason + budget persistence for operator UX.
	SetEndedReason(ctx context.Context, sessionID, reason string) error
	SetBudget(ctx context.Context, sessionID string, maxCost float64, maxTokens, timeoutSec int64) error

	// Cross-process message inject (foundation for `crush sessions inject`).
	// CreatePendingInject enqueues a signal row asking whichever process is
	// currently running the session to splice messageID into its live prompt.
	// DrainPendingInjects is called from PrepareStep to consume those rows.
	CreatePendingInject(ctx context.Context, inject PendingInject) error
	DrainPendingInjects(ctx context.Context, sessionID string) ([]PendingInject, bool, error)
	// ConsumeInterruptInject reads and deletes (delete-after-read, in one
	// transaction) the OLDEST interrupt=true pending_injects row for
	// sessionID, returning it. Counterpart to DrainPendingInjects, which
	// deliberately leaves interrupt rows untouched: those are owned by the
	// coordinator's interrupt ticker, which calls this to pick one up, cancel
	// the running turn, and requeue the already-persisted message. Returns
	// (nil, nil) when no interrupt row is pending.
	ConsumeInterruptInject(ctx context.Context, sessionID string) (*PendingInject, error)

	// Agent tool session management
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	IsAgentToolSession(sessionID string) bool
}

type service struct {
	*pubsub.Broker[Session]
	db *sql.DB
	q  *db.Queries
}

func (s *service) Create(ctx context.Context, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:    uuid.New().String(),
		Title: title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateWithID(ctx context.Context, id, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:    id,
		Title: title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              "title-" + parentSessionID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           "Generate a title",
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	dbSession, err := qtx.GetSessionByID(ctx, id)
	if err != nil {
		return err
	}
	if err = qtx.DeleteSessionMessages(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session messages: %w", err)
	}
	if err = qtx.DeleteSessionFiles(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session files: %w", err)
	}
	if err = qtx.DeleteSession(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.DeletedEvent, session)
	return nil
}

func (s *service) Get(ctx context.Context, id string) (Session, error) {
	dbSession, err := s.q.GetSessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
}

func (s *service) GetLast(ctx context.Context) (Session, error) {
	dbSession, err := s.q.GetLastSession(ctx)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
}

// Save overwrites title/tokens/summary/todos for a session. Cost is NOT
// written by this call (the underlying UpdateSession SQL was reshaped to
// exclude it) so a stale in-memory session.Cost cannot stomp cost that
// other goroutines accrued concurrently. Use IncrementCost for cost
// mutations.
//
// Fork patch (concurrency): the upstream Save also wrote the cost
// column. See CHANGELOG.fork.md (Section 4.I).
func (s *service) Save(ctx context.Context, session Session) (Session, error) {
	todosJSON, err := marshalTodos(session.Todos)
	if err != nil {
		return Session{}, err
	}

	deletedTodosJSON, err := marshalDeletedTodos(session.DeletedTodos)
	if err != nil {
		return Session{}, err
	}

	dbSession, err := s.q.UpdateSession(ctx, db.UpdateSessionParams{
		ID:               session.ID,
		Title:            session.Title,
		PromptTokens:     session.PromptTokens,
		CompletionTokens: session.CompletionTokens,
		SummaryMessageID: sql.NullString{
			String: session.SummaryMessageID,
			Valid:  session.SummaryMessageID != "",
		},
		Todos: sql.NullString{
			String: todosJSON,
			Valid:  todosJSON != "",
		},
		DeletedTodos: deletedTodosJSON,
	})
	if err != nil {
		return Session{}, err
	}
	session = s.fromDBItem(dbSession)
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

// IncrementCost adds delta to the session cost atomically. See interface
// doc on Service.IncrementCost for rationale.
func (s *service) IncrementCost(ctx context.Context, sessionID string, delta float64) (Session, error) {
	if delta == 0 {
		return s.Get(ctx, sessionID)
	}
	dbSession, err := s.q.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:   sessionID,
		Cost: delta,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

// UpdateTitleAndUsage updates only the title and usage fields atomically.
// This is safer than fetching, modifying, and saving the entire session.
func (s *service) UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error {
	return s.q.UpdateSessionTitleAndUsage(ctx, db.UpdateSessionTitleAndUsageParams{
		ID:               sessionID,
		Title:            title,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		Cost:             cost,
	})
}

// UpdateSystemPrompt saves a custom system prompt for a session.
func (s *service) UpdateSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	if err := s.q.UpdateSessionSystemPrompt(ctx, db.UpdateSessionSystemPromptParams{
		ID:           sessionID,
		SystemPrompt: prompt,
	}); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateModels updates the models associated with a session.
func (s *service) UpdateModels(ctx context.Context, sessionID, largeProvider, largeModel, smallProvider, smallModel string) error {
	err := s.q.UpdateSessionModels(ctx, db.UpdateSessionModelsParams{
		ID:                 sessionID,
		LargeModelProvider: sql.NullString{String: largeProvider, Valid: largeProvider != ""},
		LargeModelID:       sql.NullString{String: largeModel, Valid: largeModel != ""},
		SmallModelProvider: sql.NullString{String: smallProvider, Valid: smallProvider != ""},
		SmallModelID:       sql.NullString{String: smallModel, Valid: smallModel != ""},
	})
	if err != nil {
		return err
	}

	// Publish an update event so the UI gets the new session state
	sess, err := s.Get(ctx, sessionID)
	if err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// UpdateReasoningEffort updates the reasoning effort for large and small models.
func (s *service) UpdateReasoningEffort(ctx context.Context, sessionID, largeEffort, smallEffort string) error {
	err := s.q.UpdateSessionReasoningEffort(ctx, db.UpdateSessionReasoningEffortParams{
		ID:                        sessionID,
		LargeModelReasoningEffort: sql.NullString{String: largeEffort, Valid: largeEffort != ""},
		SmallModelReasoningEffort: sql.NullString{String: smallEffort, Valid: smallEffort != ""},
	})
	if err != nil {
		return err
	}

	// Publish an update event so the UI gets the new session state
	sess, err := s.Get(ctx, sessionID)
	if err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// Rename updates only the title of a session without touching updated_at or
// usage fields.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	return s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	})
}

// ListSubSessions implementation: thin wrapper around the sqlc-
// generated query. Returns an empty slice when no sub-sessions exist.
func (s *service) ListSubSessions(ctx context.Context, parentSessionID string) ([]Session, error) {
	dbSessions, err := s.q.ListSubSessions(ctx, sql.NullString{
		String: parentSessionID,
		Valid:  parentSessionID != "",
	})
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
	}
	return sessions, nil
}

func (s *service) List(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.q.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
	}
	return sessions, nil
}

// Fork merge note (origin/main 2736e487 "fix(ui): mark estimated context
// usage" + 9595d1f0 "fix(session): preserve estimated usage marker"):
// upstream added applyEstimatedUsageState / setEstimatedUsageState /
// clearEstimatedUsageState as backend infrastructure for their TUI's
// "estimated context usage" marker. Rejected — the whole feature drives
// a TUI widget we do not ship; our WebUI handles usage display via the
// WebSocket events stream (internal/server/events.go) without per-session
// estimated-state tracking. See CHANGELOG.fork.md Section 2.
func (s *service) ListAll(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, parent_session_id, title, message_count,
		prompt_tokens, completion_tokens, cost, updated_at, created_at,
		summary_message_id, todos,
		large_model_provider, large_model_id,
		small_model_provider, small_model_id,
		system_prompt, yolo_enabled,
		large_model_reasoning_effort, small_model_reasoning_effort,
		cancel_requested,
		COALESCE(ended_reason, ''), COALESCE(budget_max_cost, 0),
		COALESCE(budget_max_tokens, 0), COALESCE(budget_timeout_sec, 0)
		FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var item db.Session
		var cancelRequested int64
		var endedReason string
		var budgetMaxCost float64
		var budgetMaxTokens, budgetTimeoutSec int64
		if err := rows.Scan(
			&item.ID, &item.ParentSessionID, &item.Title, &item.MessageCount,
			&item.PromptTokens, &item.CompletionTokens, &item.Cost,
			&item.UpdatedAt, &item.CreatedAt, &item.SummaryMessageID, &item.Todos,
			&item.LargeModelProvider, &item.LargeModelID,
			&item.SmallModelProvider, &item.SmallModelID,
			&item.SystemPrompt, &item.YoloEnabled,
			&item.LargeModelReasoningEffort, &item.SmallModelReasoningEffort,
			&cancelRequested,
			&endedReason, &budgetMaxCost, &budgetMaxTokens, &budgetTimeoutSec,
		); err != nil {
			return nil, err
		}
		sess := s.fromDBItem(item)
		sess.CancelRequested = cancelRequested != 0
		sess.EndedReason = endedReason
		sess.BudgetMaxCost = budgetMaxCost
		sess.BudgetMaxTokens = budgetMaxTokens
		sess.BudgetTimeoutSec = budgetTimeoutSec
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s service) fromDBItem(item db.Session) Session {
	todos, err := unmarshalTodos(item.Todos.String)
	if err != nil {
		slog.Error("Failed to unmarshal todos", "session_id", item.ID, "error", err)
	}
	deletedTodos, err := unmarshalDeletedTodos(item.DeletedTodos)
	if err != nil {
		slog.Error("Failed to unmarshal deleted_todos", "session_id", item.ID, "error", err)
	}
	return Session{
		ID:               item.ID,
		ParentSessionID:  item.ParentSessionID.String,
		Title:            item.Title,
		MessageCount:     item.MessageCount,
		PromptTokens:     item.PromptTokens,
		CompletionTokens: item.CompletionTokens,
		SummaryMessageID: item.SummaryMessageID.String,
		Cost:             item.Cost,
		Todos:            todos,
		DeletedTodos:     deletedTodos,
		CreatedAt:        item.CreatedAt,
		UpdatedAt:        item.UpdatedAt,

		LargeModelProvider:        item.LargeModelProvider.String,
		LargeModelID:              item.LargeModelID.String,
		LargeModelReasoningEffort: item.LargeModelReasoningEffort.String,
		SmallModelProvider:        item.SmallModelProvider.String,
		SmallModelID:              item.SmallModelID.String,
		SmallModelReasoningEffort: item.SmallModelReasoningEffort.String,

		SystemPrompt: item.SystemPrompt,
		YoloEnabled:  item.YoloEnabled != 0,
	}
}

// RequestCancel sets the cancel_requested flag for a session so a
// running agent (possibly in a different process) stops gracefully.
func (s *service) RequestCancel(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 1 WHERE id = ?",
		sessionID,
	)
	return err
}

// IsCancelRequested checks whether a cancel signal is set on the session.
func (s *service) IsCancelRequested(ctx context.Context, sessionID string) (bool, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		"SELECT cancel_requested FROM sessions WHERE id = ?",
		sessionID,
	).Scan(&v)
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// ClearCancelRequest resets the cancel_requested flag. Called when a
// new run starts so a stale flag from a previous run does not kill it.
func (s *service) ClearCancelRequest(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET cancel_requested = 0 WHERE id = ?",
		sessionID,
	)
	return err
}

func (s *service) SetEndedReason(ctx context.Context, sessionID, reason string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET ended_reason = ?, updated_at = strftime('%s', 'now') WHERE id = ?",
		reason, sessionID,
	)
	return err
}

func (s *service) SetBudget(ctx context.Context, sessionID string, maxCost float64, maxTokens, timeoutSec int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET budget_max_cost = ?, budget_max_tokens = ?, budget_timeout_sec = ?,
		 updated_at = strftime('%s', 'now') WHERE id = ?`,
		maxCost, maxTokens, timeoutSec, sessionID,
	)
	return err
}

// PendingInject is one row of the cross-process inject queue. It is a
// signal pointing at an already-created messages row (MessageID); Content is
// carried only for debugging/logging. Interrupt distinguishes a plain merge
// (false) from an interrupt-style inject (true) owned by the interrupt
// ticker.
type PendingInject struct {
	ID        string
	SessionID string
	MessageID string
	Content   string
	Interrupt bool
	CreatedAt int64
}

// CreatePendingInject enqueues a cross-process inject signal for sessionID.
// The caller (e.g. `crush sessions inject`) is responsible for having
// already created the referenced messages row so it is immediately visible
// in the web UI; this only records the request to splice it into the live
// prompt of whatever process is running the session.
func (s *service) CreatePendingInject(ctx context.Context, inject PendingInject) error {
	if inject.ID == "" {
		inject.ID = uuid.NewString()
	}
	if inject.CreatedAt == 0 {
		inject.CreatedAt = time.Now().Unix()
	}
	interrupt := int64(0)
	if inject.Interrupt {
		interrupt = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inject.ID, inject.SessionID, inject.MessageID, inject.Content, interrupt, inject.CreatedAt,
	)
	return err
}

// DrainPendingInjects consumes the non-interrupt (interrupt = 0) inject rows
// for sessionID, deleting them in the same transaction (delete-after-read),
// and returns them ordered oldest-first for merging into the current prompt.
// The second return value reports whether an interrupt (interrupt = 1) row is
// also pending; those rows are NOT returned or deleted here — they are owned
// by the interrupt ticker, which is expected to consume them before the next
// PrepareStep. Reporting their presence lets PrepareStep log a defensive
// warning if one slipped through.
//
// SQLite serialises writers, so there is no cross-process race; the enclosing
// transaction guards against two goroutines inside this process draining the
// same rows concurrently.
func (s *service) DrainPendingInjects(ctx context.Context, sessionID string) ([]PendingInject, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects WHERE session_id = ? ORDER BY created_at ASC`,
		sessionID,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var (
		merge        []PendingInject
		consumedIDs  []string
		hasInterrupt bool
	)
	for rows.Next() {
		var (
			pi        PendingInject
			interrupt int64
		)
		if scanErr := rows.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt); scanErr != nil {
			return nil, false, scanErr
		}
		if interrupt != 0 {
			pi.Interrupt = true
			hasInterrupt = true
			continue
		}
		merge = append(merge, pi)
		consumedIDs = append(consumedIDs, pi.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	for _, id := range consumedIDs {
		if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, id); delErr != nil {
			return nil, false, delErr
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return merge, hasInterrupt, nil
}

// ConsumeInterruptInject — see Service interface doc. It selects the oldest
// interrupt row, deletes it in the same transaction, and returns it. One
// interrupt event = one cancel+requeue by the caller; consuming a single row
// per call keeps that mapping crisp even if several interrupt rows piled up.
func (s *service) ConsumeInterruptInject(ctx context.Context, sessionID string) (*PendingInject, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		pi        PendingInject
		interrupt int64
	)
	row := tx.QueryRowContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects
		 WHERE session_id = ? AND interrupt = 1
		 ORDER BY created_at ASC LIMIT 1`,
		sessionID,
	)
	if scanErr := row.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, scanErr
	}
	pi.Interrupt = interrupt != 0

	if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, pi.ID); delErr != nil {
		return nil, delErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &pi, nil
}

func marshalTodos(todos []Todo) (string, error) {
	if len(todos) == 0 {
		return "", nil
	}
	data, err := json.Marshal(todos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTodos(data string) ([]Todo, error) {
	if data == "" {
		return []Todo{}, nil
	}
	var todos []Todo
	if err := json.Unmarshal([]byte(data), &todos); err != nil {
		return []Todo{}, err
	}
	return todos, nil
}

func marshalDeletedTodos(contents []string) (string, error) {
	if len(contents) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(contents)
	if err != nil {
		return "[]", err
	}
	return string(data), nil
}

func unmarshalDeletedTodos(data string) ([]string, error) {
	if data == "" || data == "[]" {
		return []string{}, nil
	}
	var contents []string
	if err := json.Unmarshal([]byte(data), &contents); err != nil {
		return []string{}, err
	}
	return contents, nil
}

func NewService(q *db.Queries, conn *sql.DB) Service {
	broker := pubsub.NewBroker[Session]()
	return &service{
		Broker: broker,
		db:     conn,
		q:      q,
	}
}

// CreateAgentToolSessionID creates a session ID for agent tool sessions using the format "messageID$$toolCallID"
func (s *service) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

// ParseAgentToolSessionID parses an agent tool session ID into its components
func (s *service) ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsAgentToolSession checks if a session ID follows the agent tool session format
func (s *service) IsAgentToolSession(sessionID string) bool {
	_, _, ok := s.ParseAgentToolSessionID(sessionID)
	return ok
}
