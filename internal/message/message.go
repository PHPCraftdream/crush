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
	ListUserMessages(ctx context.Context, sessionID string) ([]Message, error)
	ListAllUserMessages(ctx context.Context) ([]Message, error)
	Delete(ctx context.Context, id string) error
	DeleteSessionMessages(ctx context.Context, sessionID string) error
	SetPinned(ctx context.Context, id string, pinned bool) error
}

type service struct {
	*pubsub.Broker[Message]
	q db.Querier
}

func NewService(q db.Querier) Service {
	return &service{
		Broker: pubsub.NewBroker[Message](),
		q:      q,
	}
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
	s.Publish(pubsub.DeletedEvent, message.Clone())
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
	if f := message.FinishPart(); f != nil && !f.Partial {
		finishedAt.Int64 = f.Time
		finishedAt.Valid = true
	}
	err = s.q.UpdateMessage(ctx, db.UpdateMessageParams{
		ID:         message.ID,
		Parts:      string(parts),
		FinishedAt: finishedAt,
	})
	if err != nil {
		return err
	}
	message.UpdatedAt = time.Now().Unix()
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	s.Publish(pubsub.UpdatedEvent, message.Clone())
	return nil
}

func (s *service) Get(ctx context.Context, id string) (Message, error) {
	dbMessage, err := s.q.GetMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(dbMessage)
}

func (s *service) List(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.q.ListMessagesBySession(ctx, sessionID)
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

func (s *service) ListUserMessages(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.q.ListUserMessagesBySession(ctx, sessionID)
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
	dbMessages, err := s.q.ListAllUserMessages(ctx)
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
	}, nil
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
	s.Publish(pubsub.UpdatedEvent, msg.Clone())
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
