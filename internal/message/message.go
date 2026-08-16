package message

// Fork patch: this file diverges from upstream in two ways.
//
//  1. CreateMessageParams adds `ReasoningEffort` and `Hidden`; the Service
//     interface adds `Notify` (DB-less pubsub for streaming deltas) and
//     `SetPinned`. Matching DB migrations live under
//     `internal/db/migrations/20260310*`, `20260311*`, `20260313000001`.
//
//  2. We removed upstream's debounced/coalesced update layer (defaultUpdate-
//     Debounce, pendingState, Flush/FlushAll). Our streaming path uses the
//     in-process pubsub (Notify) for high-frequency UI updates and falls back
//     to synchronous Update for terminal-state writes — this matches the
//     latest-snapshot ticker in `internal/agent/agent.go`.
//
// Before merging upstream changes here: read CHANGELOG.fork.md section 2
// ("internal/message/message.go") and section 4.C (DB migrations).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

type CreateMessageParams struct {
	Role             MessageRole
	Parts            []ContentPart
	Model            string
	Provider         string
	ReasoningEffort  string // "low", "medium", "high", or "max" - reasoning effort for Claude models
	IsSummaryMessage bool
	// Hidden marks the message as invisible in the UI. Used for silent
	// background summaries that provide context to the LLM without cluttering
	// the conversation view.
	Hidden bool
	// AutoResumed marks a user message that was created by Phase 4 autonomous
	// idle-resume, not typed by a human; surfaced as a web badge.
	AutoResumed bool
	// BackgroundJobNotice marks a system-injected background-job-completion
	// notice so the web renders it as a notice, not a human message.
	BackgroundJobNotice bool
}

type Service interface {
	pubsub.Subscriber[Message]
	Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error)
	Update(ctx context.Context, message Message) error
	// Notify publishes a message update to the UI without writing to the database.
	// Use this for high-frequency streaming updates where DB durability is not
	// required on every token; call Update at the end to persist the final state.
	Notify(message Message)
	Get(ctx context.Context, id string) (Message, error)
	List(ctx context.Context, sessionID string) ([]Message, error)
	// ListPaginated returns at most limit messages for a session, newest first
	// (DESC by created_at), skipping the first offset rows. It is the paginated
	// counterpart to List so callers can read just the window they need instead
	// of decoding the entire history.
	//
	// WARNING: calling this back-to-back with a separate Count call is racy
	// under concurrent inserts (the two are independent round trips, so a
	// message written in between can make the two disagree) - callers that
	// need a self-consistent (window, total) pair, most notably reading a
	// LIVE, still-growing transcript, should use ListPaginatedSnapshot instead.
	ListPaginated(ctx context.Context, sessionID string, limit, offset int) ([]Message, error)
	// Count returns the total number of messages stored for a session. Used by
	// callers that page with ListPaginated to render accurate "N earlier
	// omitted" markers without loading the full history.
	Count(ctx context.Context, sessionID string) (int64, error)
	// ListPaginatedSnapshot is the race-free counterpart to calling
	// ListPaginated and Count separately: it returns a window of at most limit
	// messages, newest-first, skipping the first offset rows, TOGETHER with
	// the total message count - both computed from a single consistent
	// snapshot pinned at the moment this call begins.
	//
	// This matters specifically for offset-based paging over a session whose
	// message list can grow WHILE it is being read (the primary use case:
	// read_delegation_transcript.go observing a live sub-agent delegation).
	// Offset counts back from the newest end of a DESC-ordered list, so it is
	// only meaningful relative to a fixed snapshot of "what the newest row
	// is" - if new messages are inserted between reading the total and
	// reading the window, a numeric offset silently refers to a different
	// logical position than the one the caller computed it against,
	// producing skipped or duplicated messages on the next read of a
	// still-growing transcript. ListPaginatedSnapshot pins the boundary row
	// (the row `offset` positions back from the newest, as of one atomic
	// query) FIRST, then fetches the window strictly at-or-before that
	// boundary via a keyset filter - so any message inserted after this call
	// begins is correctly excluded from both the returned total and the
	// returned window, rather than shifting either one.
	ListPaginatedSnapshot(ctx context.Context, sessionID string, limit, offset int) (window []Message, total int64, err error)
	ListUserMessages(ctx context.Context, sessionID string) ([]Message, error)
	ListAllUserMessages(ctx context.Context) ([]Message, error)
	Delete(ctx context.Context, id string) error
	DeleteSessionMessages(ctx context.Context, sessionID string) error
	SetPinned(ctx context.Context, id string, pinned bool) error
	// SetUsage records this message's token accounting and prompt-cache
	// breakdown (task #469). Called by the agent right after the step's cost
	// delta is computed; separate from Update because the Finish part is
	// appended before usage is known.
	//
	// Callers should treat a failure as non-fatal: statistics must never
	// abort a turn.
	SetUsage(ctx context.Context, id string, usage TokenUsage) error
	// UsageBySession returns per-model token/cache/cost aggregates for a
	// session, together with the number of assistant messages that carry no
	// usage at all, so a caller can state what its numbers were computed
	// over instead of implying full coverage.
	UsageBySession(ctx context.Context, sessionID string) (UsageReport, error)
	// UsageByModelInRange aggregates usage across ALL sessions in a time
	// window (Unix seconds), grouped by the model that produced each message
	// rather than by the session's current model.
	UsageByModelInRange(ctx context.Context, since, until int64) (UsageReport, error)
	// UsageByDayInRange is the same window bucketed by local calendar day.
	UsageByDayInRange(ctx context.Context, since, until int64) ([]DayUsage, error)
}

type service struct {
	*pubsub.Broker[Message]
	q db.Querier

	// qRead backs the standalone, read-only hot paths (Get, List,
	// ListPaginated, Count, ListUserMessages, ListAllUserMessages) that
	// don't need read-your-own-write consistency with a subsequent write in
	// the same call — most notably transcript pagination, which can walk a
	// large OFFSET and would otherwise sit in the same single-connection
	// queue as every in-flight agent message write. Defaults to q (see
	// NewService), so callers that don't opt into a separate reader get
	// today's serialized behavior unchanged.
	qRead db.Querier
}

func NewService(q db.Querier) Service {
	return &service{
		Broker: pubsub.NewBroker[Message](),
		q:      q,
		qRead:  q,
	}
}

// NewServiceWithReader is NewService plus a separate read-only Querier
// (qRead) for the standalone hot read paths documented on the service
// struct. Production wiring (internal/app.New) passes db.ConnectRead's
// WAL-mode read-only pool here so transcript pagination and message listing
// run concurrently with the single writer connection instead of queuing
// behind it. Passing a nil qRead behaves exactly like NewService.
//
// qRead is deliberately the concrete *db.Queries type, not the db.Querier
// interface: a nil *db.Queries boxed into a db.Querier interface value is
// itself non-nil (Go's typed-nil-in-interface gotcha — the interface has a
// type descriptor even though the underlying pointer is nil), which would
// make the "if qRead != nil" fallback below never fire. That is exactly
// the case internal/app.New hits whenever db.ConnectRead fails and
// deliberately passes a nil *db.Queries reader intending to fall back to
// the writer (see app.go's doc comment on that path) — with the interface
// type, this method would silently install the typed-nil as qRead instead
// of falling back, and every subsequent s.qRead.* call would nil-dereference.
func NewServiceWithReader(q db.Querier, qRead *db.Queries) Service {
	svc := NewService(q).(*service)
	if qRead != nil {
		svc.qRead = qRead
	}
	return svc
}

func (s *service) Delete(ctx context.Context, id string) error {
	message, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	err = s.q.DeleteMessage(ctx, message.ID)
	if err != nil {
		return err
	}
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	//
	// Deletion is a terminal, low-frequency event: if it's silently
	// dropped because a subscriber's channel is momentarily full, the UI
	// keeps showing a message that no longer exists in the DB, with no
	// further event ever arriving to correct it (unlike Update, nothing
	// else republishes this message's state afterward). Use
	// PublishMustDeliver so the drop only happens after a bounded wait,
	// not on the first full buffer.
	s.PublishMustDeliver(ctx, pubsub.DeletedEvent, message.Clone())
	return nil
}

func (s *service) Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error) {
	if params.Role != Assistant {
		params.Parts = append(params.Parts, Finish{
			Reason: "stop",
		})
	}
	partsJSON, err := marshalParts(params.Parts)
	if err != nil {
		return Message{}, err
	}
	isSummary := int64(0)
	if params.IsSummaryMessage {
		isSummary = 1
	}
	hidden := int64(0)
	if params.Hidden {
		hidden = 1
	}
	autoResumed := int64(0)
	if params.AutoResumed {
		autoResumed = 1
	}
	backgroundJobNotice := int64(0)
	if params.BackgroundJobNotice {
		backgroundJobNotice = 1
	}
	dbMessage, err := s.q.CreateMessage(ctx, db.CreateMessageParams{
		ID:                  uuid.New().String(),
		SessionID:           sessionID,
		Role:                string(params.Role),
		Parts:               string(partsJSON),
		Model:               sql.NullString{String: string(params.Model), Valid: true},
		Provider:            sql.NullString{String: params.Provider, Valid: params.Provider != ""},
		ReasoningEffort:     sql.NullString{String: params.ReasoningEffort, Valid: params.ReasoningEffort != ""},
		IsSummaryMessage:    isSummary,
		Hidden:              hidden,
		AutoResumed:         autoResumed,
		BackgroundJobNotice: backgroundJobNotice,
	})
	if err != nil {
		return Message{}, err
	}
	message, err := s.fromDBItem(dbMessage)
	if err != nil {
		return Message{}, err
	}
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	//
	// Create is deliberately left on best-effort Publish: a brand-new
	// message is (outside of Hidden/summary rows) about to be updated
	// repeatedly as the assistant streams, via Notify/Update below,
	// which already use must-deliver where it matters. If this
	// CreatedEvent is dropped under contention, the next Update quickly
	// re-establishes the message for subscribers; there's no terminal
	// state here worth blocking the caller for.
	s.Publish(pubsub.CreatedEvent, message.Clone())
	return message, nil
}

func (s *service) DeleteSessionMessages(ctx context.Context, sessionID string) error {
	messages, err := s.List(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.SessionID == sessionID {
			err = s.Delete(ctx, message.ID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *service) Notify(message Message) {
	s.Publish(pubsub.UpdatedEvent, message.Clone())
}

func (s *service) Update(ctx context.Context, message Message) error {
	parts, err := marshalParts(message.Parts)
	if err != nil {
		return err
	}
	finishedAt := sql.NullInt64{}
	// Fork patch: batch 8 — a Partial finish is NOT a real finish;
	// finished_at stays NULL so the row is still "in progress".
	// The auto-checkpoint ticker uses this to persist mid-stream state
	// without confusing IsFinished / recovery.
	finish := message.FinishPart()
	partialCheckpoint := finish != nil && finish.Partial
	if finish != nil && !finish.Partial {
		finishedAt.Int64 = finish.Time
		finishedAt.Valid = true
	}

	// P0-4 fix: Use conditional update for partial checkpoints to prevent
	// a hung checkpoint from overwriting a terminal finish after unblocking.
	// If the DB already has a non-partial finish (finished_at IS NOT NULL),
	// UpdateMessageIfNotTerminal skips the update (0 rows affected), which
	// is the correct outcome: the terminal state wins, the stale checkpoint
	// is safely discarded.
	var rowsAffected int64
	var dbErr error
	if partialCheckpoint {
		// The DB update is conditional: it only touches rows that still
		// have finished_at IS NULL (no terminal finish yet). If a real
		// terminal finish landed concurrently, this returns 0 rows affected,
		// meaning the stale partial checkpoint lost and must NOT be published
		// to avoid reverting the UI to an incomplete state.
		rowsAffected, dbErr = s.q.UpdateMessageIfNotTerminal(ctx, db.UpdateMessageIfNotTerminalParams{
			ID:         message.ID,
			Parts:      string(parts),
			FinishedAt: finishedAt,
		})
	} else {
		dbErr = s.q.UpdateMessage(ctx, db.UpdateMessageParams{
			ID:         message.ID,
			Parts:      string(parts),
			FinishedAt: finishedAt,
		})
		rowsAffected = 1 // Terminal update always wins
	}
	if dbErr != nil {
		return dbErr
	}
	message.UpdatedAt = time.Now().Unix()
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	//
	// Delivery semantics split on whether this is a terminal write or a
	// mid-stream checkpoint snapshot:
	//
	//   - Terminal (real non-Partial Finish, tool-result flush,
	//     summary): PublishMustDeliver, so a momentarily full
	//     subscriber buffer doesn't silently eat the final state. The
	//     caller is bounded by mustDeliverTimeout per subscriber.
	//
	//   - Partial checkpoint (Finish.Partial == true, written by the
	//     auto-checkpoint ticker every ~2s during streaming):
	//     best-effort Publish, but ONLY if the update actually touched
	//     the DB (rowsAffected > 0). A stale checkpoint that returns 0 rows
	//     because a terminal finish already landed must NOT be published,
	//     or the UI will show the last event as an incomplete partial snapshot.
	//     Losing a tick for a slow subscriber is harmless because the next
	//     update re-establishes current state. Routing it through
	//     PublishMustDeliver would make every ~2s checkpoint pay the
	//     full bounded-blocking wait per slow subscriber for nothing.
	if partialCheckpoint {
		// P0-2: Only publish partial checkpoint if we actually updated the DB.
		// If rowsAffected == 0, a terminal finish already won and publishing
		// a stale partial would revert the UI to an incomplete state.
		if rowsAffected > 0 {
			s.Publish(pubsub.UpdatedEvent, message.Clone())
		}
	} else {
		s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, message.Clone())
	}
	return nil
}

func (s *service) Get(ctx context.Context, id string) (Message, error) {
	dbMessage, err := s.qRead.GetMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(dbMessage)
}

func (s *service) List(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.qRead.ListMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListPaginated(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	dbMessages, err := s.qRead.ListMessagesBySessionPaginated(ctx, db.ListMessagesBySessionPaginatedParams{
		SessionID: sessionID,
		Limit:     int64(limit),
		Offset:    int64(offset),
	})
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) Count(ctx context.Context, sessionID string) (int64, error) {
	return s.qRead.CountMessagesBySession(ctx, sessionID)
}

// ListPaginatedSnapshot implements the race-free (window, total) pairing
// documented on the Service interface. See that doc comment for the
// motivating race; this comment covers the implementation.
//
// Step 1 pins a single high-water-mark snapshot: GetTranscriptWindowCursor
// returns, from ONE query execution, both the session's total message count
// and the identity (created_at, rowid) of the row `offset` positions back
// from the newest message - i.e. exactly the first row the caller's window
// should contain. Any message inserted after this query executes is
// necessarily newer than that pinned row and is therefore correctly excluded
// from everything that follows, however many further round trips it takes.
//
// Step 2 fetches the window using that pinned boundary as an inclusive
// keyset cursor, split into two queries because sqlc's SQLite catalog cannot
// validate a bare `rowid` reference in a WHERE clause (confirmed by hand;
// see the doc comments on GetTranscriptWindowCursor/
// ListMessagesBySessionAtCreatedAt in internal/db/sql/messages.sql for the
// full explanation) - so the "older seconds" and "tied-second" halves of the
// keyset filter are two separate queries, both filtering only on the real
// `created_at` column, with the rowid tiebreaker applied in Go for the tied
// second only (which is bounded to however many messages share one second,
// not the whole table).
func (s *service) ListPaginatedSnapshot(ctx context.Context, sessionID string, limit, offset int) ([]Message, int64, error) {
	if limit <= 0 {
		// Defensive: the only caller (read_delegation_transcript.go's
		// clampTranscriptWindow) always clamps to a positive default before
		// calling in, so this is unreached in production today. But
		// make([]db.Message, 0, limit) below panics for a negative limit,
		// and limit == 0 would otherwise still return exactly one row (the
		// tied-second loop appends before checking len(dbMessages) >=
		// limit) instead of the zero rows a caller asking for "0 messages"
		// should get.
		total, err := s.qRead.CountMessagesBySession(ctx, sessionID)
		if err != nil {
			return nil, 0, err
		}
		return []Message{}, total, nil
	}

	cursor, err := s.qRead.GetTranscriptWindowCursor(ctx, db.GetTranscriptWindowCursorParams{
		SessionID: sessionID,
		Offset:    int64(offset),
	})
	if errors.Is(err, sql.ErrNoRows) {
		// offset is at or past the end of the session's history: no boundary
		// row exists, so the window is empty. Total still needs a value -
		// fall back to a plain count (no window race to guard against when
		// the window itself is empty).
		total, cerr := s.qRead.CountMessagesBySession(ctx, sessionID)
		if cerr != nil {
			return nil, 0, cerr
		}
		return []Message{}, total, nil
	}
	if err != nil {
		return nil, 0, err
	}

	// Tied-second half first: every message sharing the boundary's exact
	// created_at second, newest-rowid-first, keeping only rowid <= the
	// boundary's own rowid (the boundary row itself must be INCLUDED - it is
	// the first row of the window, not an exclusive fence post).
	tied, err := s.qRead.ListMessagesBySessionAtCreatedAt(ctx, db.ListMessagesBySessionAtCreatedAtParams{
		SessionID: sessionID,
		CreatedAt: cursor.CreatedAt,
	})
	if err != nil {
		return nil, 0, err
	}

	dbMessages := make([]db.Message, 0, limit)
	for _, row := range tied {
		if row.RowID > cursor.RowID {
			continue // newer than the boundary within the same second - excluded.
		}
		dbMessages = append(dbMessages, row.Message)
		if len(dbMessages) >= limit {
			break
		}
	}

	// Older-seconds half: only queried if the tied-second rows didn't
	// already fill the window, and LIMITed to exactly what's left.
	if len(dbMessages) < limit {
		older, err := s.qRead.ListMessagesBySessionOlderThanCreatedAt(ctx, db.ListMessagesBySessionOlderThanCreatedAtParams{
			SessionID: sessionID,
			CreatedAt: cursor.CreatedAt,
			Limit:     int64(limit - len(dbMessages)),
		})
		if err != nil {
			return nil, 0, err
		}
		dbMessages = append(dbMessages, older...)
	}

	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, 0, err
		}
	}
	return messages, cursor.TotalCount, nil
}

func (s *service) ListUserMessages(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.qRead.ListUserMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListAllUserMessages(ctx context.Context) ([]Message, error) {
	dbMessages, err := s.qRead.ListAllUserMessages(ctx)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) fromDBItem(item db.Message) (Message, error) {
	parts, err := unmarshalParts([]byte(item.Parts))
	if err != nil {
		return Message{}, err
	}
	return Message{
		ID:                  item.ID,
		SessionID:           item.SessionID,
		Role:                MessageRole(item.Role),
		Parts:               parts,
		Model:               item.Model.String,
		Provider:            item.Provider.String,
		ReasoningEffort:     item.ReasoningEffort.String,
		CreatedAt:           item.CreatedAt,
		UpdatedAt:           item.UpdatedAt,
		IsSummaryMessage:    item.IsSummaryMessage != 0,
		Pinned:              item.Pinned != 0,
		Hidden:              item.Hidden != 0,
		AutoResumed:         item.AutoResumed != 0,
		BackgroundJobNotice: item.BackgroundJobNotice != 0,
		Usage:               usageFromDBItem(item),
	}, nil
}

// usageFromDBItem lifts the per-message token accounting off a row, or returns
// nil when none was recorded.
//
// total_tokens is the recorded/not-recorded marker (SetUsage always writes it
// non-NULL, even when the derived value is 0), which is the same signal the
// aggregate queries key on. Returning nil rather than a zero-valued struct
// keeps "never measured" distinguishable from "measured as zero" all the way
// out to the UI, so a message from before this feature cannot be rendered as
// a confident 0% cache hit.
func usageFromDBItem(item db.Message) *TokenUsage {
	if !item.TotalTokens.Valid {
		return nil
	}
	return &TokenUsage{
		InputTokens:         item.InputTokens.Int64,
		OutputTokens:        item.OutputTokens.Int64,
		ReasoningTokens:     item.ReasoningTokens.Int64,
		CacheCreationTokens: item.CacheCreationTokens.Int64,
		CacheReadTokens:     item.CacheReadTokens.Int64,
		TotalTokens:         item.TotalTokens.Int64,
		CostUSD:             item.CostUsd.Float64,
		Provider:            item.UsageProvider.String,
		Model:               item.UsageModel.String,
		CacheSupport:        CacheSupport(item.CacheSupport.String),
		Estimated:           item.UsageEstimated.Int64 != 0,
	}
}

func (s *service) SetPinned(ctx context.Context, id string, pinned bool) error {
	pinnedVal := int64(0)
	if pinned {
		pinnedVal = 1
	}
	err := s.q.UpdateMessagePinned(ctx, db.UpdateMessagePinnedParams{
		ID:     id,
		Pinned: pinnedVal,
	})
	if err != nil {
		return err
	}
	msg, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	// Explicit, low-frequency user action (pin/unpin) — not part of the
	// streaming hot path, so the bounded PublishMustDeliver wait is free
	// here, and the user should reliably see their own action reflected.
	s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, msg.Clone())
	return nil
}

type partType string

const (
	reasoningType  partType = "reasoning"
	textType       partType = "text"
	imageURLType   partType = "image_url"
	binaryType     partType = "binary"
	toolCallType   partType = "tool_call"
	toolResultType partType = "tool_result"
	finishType     partType = "finish"
)

type partWrapper struct {
	Type partType    `json:"type"`
	Data ContentPart `json:"data"`
}

func marshalParts(parts []ContentPart) ([]byte, error) {
	wrappedParts := make([]partWrapper, len(parts))

	for i, part := range parts {
		var typ partType

		switch part.(type) {
		case ReasoningContent:
			typ = reasoningType
		case TextContent:
			typ = textType
		case ImageURLContent:
			typ = imageURLType
		case BinaryContent:
			typ = binaryType
		case ToolCall:
			typ = toolCallType
		case ToolResult:
			typ = toolResultType
		case Finish:
			typ = finishType
		default:
			return nil, fmt.Errorf("unknown part type: %T", part)
		}

		wrappedParts[i] = partWrapper{
			Type: typ,
			Data: part,
		}
	}
	return json.Marshal(wrappedParts)
}

func unmarshalParts(data []byte) ([]ContentPart, error) {
	temp := []json.RawMessage{}

	if err := json.Unmarshal(data, &temp); err != nil {
		return nil, err
	}

	parts := make([]ContentPart, 0)

	for _, rawPart := range temp {
		var wrapper struct {
			Type partType        `json:"type"`
			Data json.RawMessage `json:"data"`
		}

		if err := json.Unmarshal(rawPart, &wrapper); err != nil {
			return nil, err
		}

		switch wrapper.Type {
		case reasoningType:
			part := ReasoningContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case textType:
			part := TextContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case imageURLType:
			part := ImageURLContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case binaryType:
			part := BinaryContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolCallType:
			part := ToolCall{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolResultType:
			part := ToolResult{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case finishType:
			part := Finish{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			return nil, fmt.Errorf("unknown part type: %s", wrapper.Type)
		}
	}

	return parts, nil
}
