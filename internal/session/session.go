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
	// GetCallTreeActivity returns the freshest message activity anywhere in
	// rootID's call tree (rootID itself plus every descendant reachable via
	// parent_session_id) in ONE recursive-CTE query, instead of a
	// per-node Messages.List + ListSubSessions walk. ok is false when the
	// tree has no messages at all (nothing to report).
	GetCallTreeActivity(ctx context.Context, rootID string) (activity CallTreeActivity, ok bool, err error)
	// GetCallTreeActivityBatch is the batch form of GetCallTreeActivity: it
	// computes the freshest call-tree activity for EVERY id in rootIDs,
	// chunking the root list internally so a single batch can never exceed
	// SQLite's variable-parameter limit (callTreeActivityBatchChunkSize roots
	// per underlying query). Used by `sessions list`, which otherwise walked
	// the whole descendant tree of every running session individually. The
	// returned map is keyed by root session ID; roots with no activity in
	// their tree are simply absent from the map.
	GetCallTreeActivityBatch(ctx context.Context, rootIDs []string) (map[string]CallTreeActivity, error)
	SetUsage(ctx context.Context, sessionID string, promptTokens, completionTokens int64) error
	SetSummaryAndUsage(ctx context.Context, sessionID, summaryMessageID string, promptTokens, completionTokens int64) error
	SetTodos(ctx context.Context, sessionID string, todos []Todo, deletedTodos []string) error
	// IncrementCost atomically adds delta to the session's cost via an
	// additive SQL UPDATE. Always prefer this over a read-modify-write of the
	// cost column when accruing per-step or per-sub-agent cost: it is race-free
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
	// TransferChildCostToParent moves the child session's cost accrued since
	// the last transfer into the parent session, atomically in one DB
	// transaction. It reads the child's persisted parent_cost_accounted
	// ledger, charges only the delta (cost - accounted, clamped >= 0) to the
	// parent via the atomic IncrementSessionCost UPDATE, and advances the
	// child's accounted marker to its current cost — all inside one tx so a
	// crash between the parent charge and the child bookkeeping cannot leave
	// them inconsistent. Idempotent: a repeat call with no new child cost
	// charges zero. Replaces the old in-memory baseline scheme that lost cost
	// on sub-agent error paths, process restarts, and failed charges.
	TransferChildCostToParent(ctx context.Context, childSessionID, parentSessionID string) error
	UpdateModels(ctx context.Context, sessionID, largeProvider, largeModel, smallProvider, smallModel string) error
	UpdateReasoningEffort(ctx context.Context, sessionID, largeEffort, smallEffort string) error
	UpdateSystemPrompt(ctx context.Context, sessionID, prompt string) error
	Rename(ctx context.Context, id string, title string) error
	Delete(ctx context.Context, id string) error
	// ForkSession clones srcID into a brand-new session in a single DB
	// transaction: it creates a fresh session row and copies the source's
	// models, system prompt, todos, and every message. A failure at any
	// point rolls back the whole clone so no half-built fork is left for a
	// client to see; the caller receives the error instead. If title is
	// empty it defaults to "<src title> fork". Returns the committed fork.
	//
	// ForkSession is a thin wrapper around ForkSessionTx with the web fork
	// button's defaults (server-generated UUID, top-level session, every
	// message copied). Callers that need --at truncation, a caller-chosen
	// ID, or parent linkage (e.g. `crush sessions fork`) should call
	// ForkSessionTx directly instead of duplicating the transaction.
	ForkSession(ctx context.Context, srcID, title string) (Session, error)
	// ForkSessionTx is the single transactional fork implementation shared
	// by every fork entry point (web fork button, `crush sessions fork`).
	// It clones srcID into a brand-new session in one DB transaction: a
	// fresh session row copying the source's models, system prompt,
	// reasoning effort, and todos/deleted_todos, plus the first o.LimitMsgs
	// messages verbatim (all messages when o.LimitMsgs is 0). A failure at
	// any point rolls back the whole clone, so no half-built fork is ever
	// visible. Returns the committed fork and the number of messages copied.
	//
	// ForkOptions fields default as follows when left zero-valued:
	//   - NewID: a fresh uuid.New().String()
	//   - Title: "<src title> fork"
	//   - ParentID: "" (top-level session, no parent)
	//   - LimitMsgs: 0 means "copy every message"; otherwise it truncates to
	//     the first LimitMsgs messages (1-indexed) and the call fails if
	//     LimitMsgs is out of the range 1..len(source messages). An empty
	//     source (zero messages) is always a valid fork target as long as
	//     LimitMsgs is left at 0 — the range check is skipped in that case.
	//
	// Unlike ForkSession, ForkSessionTx does not publish a pubsub.CreatedEvent
	// after commit: some callers (e.g. the CLI) run in a separate process
	// from the one that will observe the fork, so publishing would be a
	// silent no-op there. Callers that need the event (the web path) publish
	// it themselves after this returns successfully.
	ForkSessionTx(ctx context.Context, srcID string, o ForkOptions) (Session, int, error)

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
	// sessionID, returning it. Used by P0-2 fix (cross-process interrupt
	// inject) to immediately consume the row and prevent duplicate
	// processing by subsequent ticks. The row is recreated in
	// startDetachedRun if the detached run fails even after retries.
	// Returns (nil, nil) when no interrupt row is pending.
	ConsumeInterruptInject(ctx context.Context, sessionID string) (*PendingInject, error)
	// DeleteInterruptInject removes a specific pending inject row by ID.
	// Used by detached interrupt runs to delete the durable pending row AFTER
	// they have confirmed execution (acquired OS lock). P0-2 fix.
	DeleteInterruptInject(ctx context.Context, injectID string) error

	// Agent tool session management
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	IsAgentToolSession(sessionID string) bool
}

type service struct {
	*pubsub.Broker[Session]
	db *sql.DB
	q  *db.Queries

	// readDB/qRead back the standalone, read-only hot paths (List, ListAll,
	// ListSubSessions, Get, GetLast, GetCallTreeActivity(Batch)) that don't
	// need read-your-own-write consistency with a subsequent write in the
	// same call. When a caller doesn't provide a separate reader (the
	// common NewService constructor, used by every test and by anything
	// that intentionally wants single-connection semantics against
	// :memory: databases), these simply alias db/q, so routing to them is
	// a no-op fallback to today's serialized behavior.
	//
	// Everything else (writes, transactions, and reads inside a
	// transaction like ForkSessionTx's GetSessionByID) intentionally keeps
	// using db/q — the single writer connection — so this split never
	// changes write semantics or read-after-write guarantees for anything
	// but the named standalone read paths.
	readDB *sql.DB
	qRead  *db.Queries
}

// CallTreeActivity is the freshest message activity found anywhere in a
// session's call tree (the session itself plus every descendant sub-agent
// session reachable via parent_session_id), as computed by the
// GetCallTreeActivity / GetCallTreeActivityBatch recursive-CTE queries.
type CallTreeActivity struct {
	// SessionID is the descendant (or root) session the activity belongs
	// to — i.e. which node in the tree produced LatestUnix.
	SessionID string
	// Role is the role of the freshest message ("assistant" / "tool" /
	// "user").
	Role string
	// LatestUnix is the newest message activity timestamp (max of
	// created_at / updated_at) across the whole tree.
	LatestUnix int64
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

// ForkOptions carries the knobs ForkSessionTx supports beyond "copy
// everything from srcID into a new top-level session". See the Service
// interface doc on ForkSessionTx for the zero-value default of each field.
type ForkOptions struct {
	// NewID is the caller-chosen ID for the forked session. Empty means
	// generate a fresh uuid.New().String().
	NewID string
	// Title is the forked session's title. Empty means "<src title> fork".
	Title string
	// ParentID sets the fork's parent_session_id, making it a child session
	// (as `crush sessions fork --child` does). Empty means top-level (no
	// parent) — the web fork button's behavior.
	ParentID string
	// LimitMsgs truncates the copy to the first LimitMsgs messages
	// (1-indexed). 0 means "copy every message". A non-zero value outside
	// 1..len(source messages) is rejected.
	LimitMsgs int
}

// ForkSession is a thin wrapper around ForkSessionTx using the web fork
// button's defaults: server-generated ID, top-level session, every message
// copied. It additionally publishes a pubsub.CreatedEvent after commit,
// since the web path and its subscribers live in the same process.
func (s *service) ForkSession(ctx context.Context, srcID, title string) (Session, error) {
	fork, _, err := s.ForkSessionTx(ctx, srcID, ForkOptions{Title: title})
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.CreatedEvent, fork)
	return fork, nil
}

// ForkSessionTx clones srcID into a brand-new session in a single DB
// transaction. It creates a fresh session row, copies the source's models,
// system prompt, reasoning effort, and todos/deleted_todos, and copies the
// first o.LimitMsgs messages (or all of them, when o.LimitMsgs is 0)
// verbatim — all inside one tx so a failure at any point (e.g. the Nth
// message copy) rolls back the new session row and every message copied so
// far. The caller gets an error and no half-built fork is left behind.
// Mirrors the transactional shape of Delete and TransferChildCostToParent.
//
// See the Service interface doc on ForkSessionTx for ForkOptions defaults
// and the pubsub-publish contract (this method does NOT publish; callers do).
func (s *service) ForkSessionTx(ctx context.Context, srcID string, o ForkOptions) (Session, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, 0, fmt.Errorf("begin fork transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	// Read the source inside the tx so the copy is consistent with itself.
	src, err := qtx.GetSessionByID(ctx, srcID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("load source session: %w", err)
	}

	srcMsgs, err := qtx.ListMessagesBySession(ctx, srcID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("list source messages: %w", err)
	}

	// LimitMsgs == 0 means "copy everything" and is always valid, including
	// against an empty source (forking a brand-new, message-less session is
	// a legitimate operation). A non-zero LimitMsgs must fall within
	// 1..len(srcMsgs).
	limit := o.LimitMsgs
	if limit == 0 {
		limit = len(srcMsgs)
	} else if limit < 1 || limit > len(srcMsgs) {
		return Session{}, 0, fmt.Errorf("--at %d is out of range (1..%d)", limit, len(srcMsgs))
	}
	srcMsgs = srcMsgs[:limit]

	resolvedTitle := o.Title
	if resolvedTitle == "" {
		resolvedTitle = src.Title + " fork"
	}

	forkID := o.NewID
	if forkID == "" {
		forkID = uuid.New().String()
	}

	createParams := db.CreateSessionParams{
		ID:    forkID,
		Title: resolvedTitle,
	}
	if o.ParentID != "" {
		createParams.ParentSessionID = sql.NullString{String: o.ParentID, Valid: true}
	}
	if _, err := qtx.CreateSession(ctx, createParams); err != nil {
		return Session{}, 0, fmt.Errorf("create forked session: %w", err)
	}

	// Copy the source's model selection, system prompt, reasoning effort,
	// and todos onto the fork via column-scoped UPDATEs routed through qtx
	// so they share the tx.
	if err := qtx.UpdateSessionModels(ctx, db.UpdateSessionModelsParams{
		LargeModelProvider: src.LargeModelProvider,
		LargeModelID:       src.LargeModelID,
		SmallModelProvider: src.SmallModelProvider,
		SmallModelID:       src.SmallModelID,
		ID:                 forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy models into fork: %w", err)
	}
	if err := qtx.UpdateSessionSystemPrompt(ctx, db.UpdateSessionSystemPromptParams{
		SystemPrompt: src.SystemPrompt,
		ID:           forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy system prompt into fork: %w", err)
	}
	if err := qtx.UpdateSessionReasoningEffort(ctx, db.UpdateSessionReasoningEffortParams{
		LargeModelReasoningEffort: src.LargeModelReasoningEffort,
		SmallModelReasoningEffort: src.SmallModelReasoningEffort,
		ID:                        forkID,
	}); err != nil {
		return Session{}, 0, fmt.Errorf("copy reasoning effort into fork: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET todos = ?, deleted_todos = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		src.Todos,
		src.DeletedTodos,
		forkID,
	); err != nil {
		return Session{}, 0, fmt.Errorf("copy todos into fork: %w", err)
	}

	// Copy every selected message verbatim. Parts is carried across as the
	// raw JSON blob (no decode/re-encode round-trip), so the fork is a
	// faithful copy of the source history. Any copy error aborts the whole
	// transaction.
	//
	// Deliberately no per-message pubsub.CreatedEvent here (unlike
	// message.Service.Create, which the old non-transactional path went
	// through): this loop writes via qtx.CreateMessage directly against the
	// tx, bypassing the message package's Service/Broker entirely, so there
	// is no message.Service handle available to publish through from inside
	// ForkSessionTx. Publishing would also need to wait until AFTER the
	// commit below (a subscriber must never observe a message row from an
	// uncommitted, possibly-rolled-back tx), which would mean re-reading
	// every copied message post-commit just to build event payloads.
	// Checked whether any subscriber actually needs incremental per-message
	// events for a fork: the only consumer is the web UI
	// (internal/server/events.go forwards message.Service's broker to
	// EventMessageCreated/Updated), and its client-side handler for the
	// session-level fork event does NOT rely on incremental message
	// events — web/src/useWS.ts's "session_created" handler unconditionally
	// calls `ws.send("load_messages", { sessionID: s.ID })` for every new
	// session (fork included), which does a full re-fetch of the session's
	// messages. So a client attached at fork time still ends up with a
	// complete, correct transcript. If a future subscriber needs
	// incremental per-message fork events, publish them AFTER tx.Commit()
	// below (loop over the re-read committed rows), not inside the tx.
	for _, m := range srcMsgs {
		if _, err := qtx.CreateMessage(ctx, db.CreateMessageParams{
			ID:                  uuid.New().String(),
			SessionID:           forkID,
			Role:                m.Role,
			Parts:               m.Parts,
			Model:               m.Model,
			Provider:            m.Provider,
			ReasoningEffort:     m.ReasoningEffort,
			IsSummaryMessage:    m.IsSummaryMessage,
			Hidden:              m.Hidden,
			AutoResumed:         m.AutoResumed,
			BackgroundJobNotice: m.BackgroundJobNotice,
		}); err != nil {
			return Session{}, 0, fmt.Errorf("copy message into fork: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Session{}, 0, fmt.Errorf("commit fork transaction: %w", err)
	}

	// Re-read the committed fork to return its final, fully-populated state.
	fork, err := s.q.GetSessionByID(ctx, forkID)
	if err != nil {
		return Session{}, 0, fmt.Errorf("reload forked session: %w", err)
	}
	return s.fromDBItem(fork), len(srcMsgs), nil
}

func (s *service) Get(ctx context.Context, id string) (Session, error) {
	dbSession, err := s.qRead.GetSessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
}

func (s *service) GetLast(ctx context.Context) (Session, error) {
	dbSession, err := s.qRead.GetLastSession(ctx)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
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

// TransferChildCostToParent — see Service.TransferChildCostToParent doc.
//
// The whole operation runs in one transaction so the parent charge and the
// child's accounted marker advance together or not at all: a crash between
// them can neither double-charge the parent nor lose the child's delta. The
// parent is always touched (even when delta is 0) so a deleted parent still
// surfaces as an error via the RETURNING clause — preserving the not-found
// semantics the previous IncrementCost(id, 0) short-circuit gave callers.
func (s *service) TransferChildCostToParent(ctx context.Context, childSessionID, parentSessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	accounting, err := qtx.GetSessionCostAccounting(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	delta := accounting.Cost - accounting.ParentCostAccounted
	if delta < 0 {
		// Should not happen (cost only grows), but never charge negative.
		delta = 0
	}

	// Always run the parent UPDATE: for delta 0 it is a no-op write, but the
	// RETURNING clause still surfaces sql.ErrNoRows if the parent was deleted
	// between the child finishing and this call.
	if _, err := qtx.IncrementSessionCost(ctx, db.IncrementSessionCostParams{
		ID:   parentSessionID,
		Cost: delta,
	}); err != nil {
		return fmt.Errorf("increment parent session cost: %w", err)
	}

	// Advance the child's accounted marker to its current cost so the next
	// call charges only newly accrued cost. Inside the same tx as the parent
	// charge, so a crash cannot leave the parent billed but the child lagging.
	if err := qtx.SetParentCostAccounted(ctx, db.SetParentCostAccountedParams{
		ID:                  childSessionID,
		ParentCostAccounted: accounting.Cost,
	}); err != nil {
		return fmt.Errorf("set child accounted cost: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transfer: %w", err)
	}

	// Publish refreshed snapshots so the UI reflects both new balances.
	if sess, err := s.Get(ctx, childSessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	if sess, err := s.Get(ctx, parentSessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
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

// SetUsage overwrites only the prompt/completion token counters for a
// session. It does not touch title, todos, summary, or cost, so it cannot
// clobber concurrent edits to those fields the way a full Save did. Used by
// the agent's per-step finalization to persist the latest context-window
// token snapshot.
func (s *service) SetUsage(ctx context.Context, sessionID string, promptTokens, completionTokens int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET prompt_tokens = ?, completion_tokens = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		promptTokens, completionTokens, sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetSummaryAndUsage overwrites summary_message_id together with the
// prompt/completion token counters in one UPDATE. Used by the summarization
// paths (manual and silent compaction) and by `sessions reset`, which must
// flip the summary pointer and reset token counters as one logical op. Like
// SetUsage it leaves title, todos, and cost untouched, so it cannot lose
// concurrent edits to those columns.
//
// NULL vs empty-string note: `sessions reset` calls this with
// summaryMessageID equal to the Go zero value to clear the pointer, which
// writes a SQL empty string rather than NULL to summary_message_id —
// unlike the old generic Save/UpdateSession path, which stored a Go
// zero-value string as NULL via sql.NullString{Valid: false}. This is
// intentionally NOT treated as a bug: every reader of SummaryMessageID
// compares it against the empty string (Session.SummaryMessageID != ""),
// and no SQL query anywhere filters or joins on
// `summary_message_id IS NULL`. An empty string and NULL are therefore
// equivalent for every consumer of this column today. Do not "fix" this
// without first auditing for a new IS NULL usage.
func (s *service) SetSummaryAndUsage(ctx context.Context, sessionID, summaryMessageID string, promptTokens, completionTokens int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET summary_message_id = ?, prompt_tokens = ?, completion_tokens = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		summaryMessageID, promptTokens, completionTokens, sessionID,
	); err != nil {
		return err
	}
	if sess, err := s.Get(ctx, sessionID); err == nil {
		s.Publish(pubsub.UpdatedEvent, sess)
	}
	return nil
}

// SetTodos overwrites the todos and deleted_todos (tombstone) columns for a
// session in one UPDATE. It leaves title, token counters, summary, and cost
// untouched, so a todos edit can no longer clobber a concurrent rename or
// agent step the way a full Save did.
func (s *service) SetTodos(ctx context.Context, sessionID string, todos []Todo, deletedTodos []string) error {
	todosJSON, err := marshalTodos(todos)
	if err != nil {
		return err
	}
	deletedTodosJSON, err := marshalDeletedTodos(deletedTodos)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET todos = ?, deleted_todos = ?, updated_at = strftime('%s', 'now') WHERE id = ?`,
		sql.NullString{String: todosJSON, Valid: todosJSON != ""},
		deletedTodosJSON,
		sessionID,
	); err != nil {
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
	dbSessions, err := s.qRead.ListSubSessions(ctx, sql.NullString{
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

// GetCallTreeActivity implementation: see the Service interface doc. A
// sql.ErrNoRows result (no messages anywhere in the tree) is reported as
// (zero-value, false, nil) rather than propagated as an error — an empty
// tree is a normal, expected state (e.g. a session that was just created),
// not a failure.
func (s *service) GetCallTreeActivity(ctx context.Context, rootID string) (CallTreeActivity, bool, error) {
	row, err := s.qRead.GetCallTreeActivity(ctx, rootID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CallTreeActivity{}, false, nil
		}
		return CallTreeActivity{}, false, err
	}
	return CallTreeActivity{
		SessionID:  row.SessionID,
		Role:       row.Role,
		LatestUnix: row.LatestUnix,
	}, true, nil
}

// callTreeActivityBatchChunkSize caps how many root IDs are passed to the
// underlying sqlc-generated GetCallTreeActivityBatch in a single query. The
// generated query expands rootIDs via sqlc.slice into one SQL parameter per
// id, so an unbounded list would eventually hit SQLite's
// SQLITE_MAX_VARIABLE_NUMBER ceiling (999 on older builds). 500 stays well
// below that with headroom for the query's other bound parameters, and keeps
// each recursive-CTE fan-out bounded to a reasonable batch. Because every
// root's tree is independent (the CTE partitions by root_session_id),
// splitting roots across chunks and merging the per-root maps is exact.
const callTreeActivityBatchChunkSize = 500

// GetCallTreeActivityBatch implementation: see the Service interface doc.
// rootIDs are split into callTreeActivityBatchChunkSize-sized chunks, each run
// as a separate underlying query, and the per-root results merged into one map.
func (s *service) GetCallTreeActivityBatch(ctx context.Context, rootIDs []string) (map[string]CallTreeActivity, error) {
	out := make(map[string]CallTreeActivity, len(rootIDs))
	if len(rootIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(rootIDs); start += callTreeActivityBatchChunkSize {
		end := start + callTreeActivityBatchChunkSize
		if end > len(rootIDs) {
			end = len(rootIDs)
		}
		rows, err := s.qRead.GetCallTreeActivityBatch(ctx, rootIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[row.RootSessionID] = CallTreeActivity{
				SessionID:  row.SessionID,
				Role:       row.Role,
				LatestUnix: row.LatestUnix,
			}
		}
	}
	return out, nil
}

func (s *service) List(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.qRead.ListSessions(ctx)
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
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, parent_session_id, title, message_count,
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

// DeleteInterruptInject removes a specific pending inject row by ID.
// Used by detached interrupt runs to delete the durable pending row AFTER
// they have confirmed execution (acquired OS lock). P0-2 fix.
func (s *service) DeleteInterruptInject(ctx context.Context, injectID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, injectID); delErr != nil {
		return delErr
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
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
		readDB: conn,
		qRead:  q,
	}
}

// NewServiceWithReader is NewService plus a separate read-only connection
// (readConn/qRead) for the standalone hot read paths documented on the
// service struct. Production wiring (internal/app.New) uses this with
// db.ConnectRead's WAL-mode read-only pool so heavy reads (call-tree CTEs,
// `sessions list`/`grep`) run concurrently with the single writer connection
// instead of queuing behind it. Every other caller (tests, and anything
// against a :memory: database where a genuinely separate reader either
// can't see the writer's data or isn't worth the complexity) should keep
// using plain NewService, which makes readDB/qRead alias the writer and so
// behaves exactly as it did before this split.
func NewServiceWithReader(q *db.Queries, conn *sql.DB, qRead *db.Queries, readConn *sql.DB) Service {
	svc := NewService(q, conn).(*service)
	if qRead != nil && readConn != nil {
		svc.qRead = qRead
		svc.readDB = readConn
	}
	return svc
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
