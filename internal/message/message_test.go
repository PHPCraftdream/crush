package message

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
)

func newTestMessageDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })

	// Run migrations
	_, err = sqlDB.ExecContext(context.Background(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			summary_message_id TEXT,
			todos TEXT,
			large_model_provider TEXT,
			large_model_id TEXT,
			large_model_reasoning_effort TEXT DEFAULT 'medium',
			small_model_provider TEXT,
			small_model_id TEXT,
			small_model_reasoning_effort TEXT DEFAULT 'medium',
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			provider TEXT,
			reasoning_effort TEXT DEFAULT 'medium',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER,
			is_summary_message INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			hidden INTEGER NOT NULL DEFAULT 0,
			auto_resumed INTEGER NOT NULL DEFAULT 0,
			background_job_notice INTEGER NOT NULL DEFAULT 0
		);
	`)
	require.NoError(t, err)

	return sqlDB, db.New(sqlDB)
}

func TestCreateMessage_WithReasoningEffort(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-123"

	t.Run("creates message with reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: "Hello"}},
			Model:           "claude-opus-1m",
			Provider:        "local-cli",
			ReasoningEffort: "high",
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, "high", msg.ReasoningEffort)
		assert.Equal(t, "claude-opus-1m", msg.Model)
		assert.Equal(t, "local-cli", msg.Provider)
	})

	t.Run("creates message with max effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: "Max effort response"}},
			Model:           "claude-sonnet-1m",
			Provider:        "local-cli",
			ReasoningEffort: "max",
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, "max", msg.ReasoningEffort)
	})

	t.Run("creates message without reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:     Assistant,
			Parts:    []ContentPart{TextContent{Text: "No effort specified"}},
			Model:    "gpt-4",
			Provider: "openai",
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Empty(t, msg.ReasoningEffort)
	})

	t.Run("supports all effort levels", func(t *testing.T) {
		levels := []string{"low", "medium", "high", "max"}
		for _, level := range levels {
			params := CreateMessageParams{
				Role:            Assistant,
				Parts:           []ContentPart{TextContent{Text: level}},
				Model:           "claude-opus-1m",
				Provider:        "anthropic",
				ReasoningEffort: level,
			}

			msg, err := svc.Create(ctx, sessionID, params)
			require.NoError(t, err, "level=%s", level)
			assert.Equal(t, level, msg.ReasoningEffort, "level=%s", level)
		}
	})

	t.Run("persists reasoning effort to database", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: "Persistence test"}},
			Model:           "claude-opus-1m",
			Provider:        "local-cli",
			ReasoningEffort: "high",
		}

		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)

		// Retrieve from database
		retrieved, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, "high", retrieved.ReasoningEffort)
		assert.Equal(t, created.Model, retrieved.Model)
		assert.Equal(t, created.Provider, retrieved.Provider)
	})
}

func TestListMessages_WithReasoningEffort(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-list"

	// Create messages with different effort levels
	efforts := []string{"low", "medium", "high", "max"}
	for i, effort := range efforts {
		params := CreateMessageParams{
			Role:            Assistant,
			Parts:           []ContentPart{TextContent{Text: effort}},
			Model:           "claude-opus-1m",
			Provider:        "local-cli",
			ReasoningEffort: effort,
			Hidden:          i > 1, // Make some hidden
		}
		_, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
	}

	// List all messages
	messages, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 4)

	// Verify effort levels are preserved
	for i, msg := range messages {
		assert.Equal(t, efforts[i], msg.ReasoningEffort)
	}
}

func TestCreateMessage_ParamsValidation(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-validation"

	t.Run("user message can have reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:            User,
			Parts:           []ContentPart{TextContent{Text: "User message"}},
			ReasoningEffort: "", // User messages typically don't have effort
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, User, msg.Role)
		assert.Empty(t, msg.ReasoningEffort)
	})

	t.Run("assistant message with all fields", func(t *testing.T) {
		params := CreateMessageParams{
			Role:             Assistant,
			Parts:            []ContentPart{TextContent{Text: "Complete message"}},
			Model:            "claude-sonnet-1m",
			Provider:         "anthropic",
			ReasoningEffort:  "medium",
			IsSummaryMessage: false,
			Hidden:           false,
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, Assistant, msg.Role)
		assert.Equal(t, "claude-sonnet-1m", msg.Model)
		assert.Equal(t, "anthropic", msg.Provider)
		assert.Equal(t, "medium", msg.ReasoningEffort)
		assert.False(t, msg.IsSummaryMessage)
		assert.False(t, msg.Hidden)
	})

	t.Run("summary message with reasoning effort", func(t *testing.T) {
		params := CreateMessageParams{
			Role:             Assistant,
			Parts:            []ContentPart{TextContent{Text: "Summary"}},
			Model:            "claude-opus-1m",
			Provider:         "local-cli",
			ReasoningEffort:  "low",
			IsSummaryMessage: true,
			Hidden:           true,
		}

		msg, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.Equal(t, "low", msg.ReasoningEffort)
		assert.True(t, msg.IsSummaryMessage)
		assert.True(t, msg.Hidden)
	})
}

func TestFinishToolCall_PreservesProviderExecuted(t *testing.T) {
	msg := &Message{
		Parts: []ContentPart{
			ToolCall{
				ID:               "call-1",
				Name:             "bash",
				Input:            `{"cmd":"ls"}`,
				ProviderExecuted: true,
			},
		},
	}

	msg.FinishToolCall("call-1")

	tc, ok := msg.Parts[0].(ToolCall)
	require.True(t, ok)
	assert.True(t, tc.Finished, "FinishToolCall should set Finished=true")
	assert.True(t, tc.ProviderExecuted, "FinishToolCall should preserve ProviderExecuted")
	assert.Equal(t, "call-1", tc.ID)
	assert.Equal(t, "bash", tc.Name)
	assert.Equal(t, `{"cmd":"ls"}`, tc.Input)
}

func TestCreateMessage_AutoResumedRoundTrip(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-auto-resumed"

	t.Run("auto-resumed flag persists and reads back true", func(t *testing.T) {
		params := CreateMessageParams{
			Role:        User,
			Parts:       []ContentPart{TextContent{Text: "Background job xyz finished"}},
			AutoResumed: true,
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.True(t, created.AutoResumed, "created message should have AutoResumed=true")

		// Round-trip through Get.
		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.True(t, got.AutoResumed, "Get should return AutoResumed=true")

		// Round-trip through List.
		listed, err := svc.List(ctx, sessionID)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.True(t, listed[0].AutoResumed, "List should return AutoResumed=true")
	})

	t.Run("default is false when flag omitted", func(t *testing.T) {
		params := CreateMessageParams{
			Role:  User,
			Parts: []ContentPart{TextContent{Text: "normal human message"}},
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.False(t, created.AutoResumed, "omitted AutoResumed should default to false")

		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, got.AutoResumed, "Get should return AutoResumed=false for default")
	})
}

func TestCreateMessage_BackgroundJobNoticeRoundTrip(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q)

	ctx := t.Context()
	sessionID := "test-session-bg-job-notice"

	t.Run("background-job-notice flag persists and reads back true", func(t *testing.T) {
		params := CreateMessageParams{
			Role:                User,
			Parts:               []ContentPart{TextContent{Text: "Background job xyz finished"}},
			BackgroundJobNotice: true,
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.True(t, created.BackgroundJobNotice, "created message should have BackgroundJobNotice=true")

		// Round-trip through Get.
		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.True(t, got.BackgroundJobNotice, "Get should return BackgroundJobNotice=true")

		// Round-trip through List.
		listed, err := svc.List(ctx, sessionID)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.True(t, listed[0].BackgroundJobNotice, "List should return BackgroundJobNotice=true")
	})

	t.Run("default is false when flag omitted", func(t *testing.T) {
		params := CreateMessageParams{
			Role:  User,
			Parts: []ContentPart{TextContent{Text: "normal human message"}},
		}
		created, err := svc.Create(ctx, sessionID, params)
		require.NoError(t, err)
		assert.False(t, created.BackgroundJobNotice, "omitted BackgroundJobNotice should default to false")

		got, err := svc.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, got.BackgroundJobNotice, "Get should return BackgroundJobNotice=false for default")
	})
}

// TestUpdate_TerminalWrite_UsesPublishMustDeliver verifies that a
// terminal Update — one whose Finish is a real (non-Partial) finish —
// does not silently drop its UpdatedEvent the moment a subscriber's
// channel is momentarily full. Unlike best-effort Publish, it must wait
// (bounded by the broker's must-deliver timeout) for buffer space, so a
// slow-draining UI subscriber still eventually sees the final message
// state instead of losing it forever. The Partial-checkpoint
// counterpart is TestUpdate_PartialCheckpoint_UsesBestEffortPublish.
//
// service embeds *pubsub.Broker[Message] directly, so we drive the
// broker's buffer to capacity through the same Subscribe channel the
// service hands out, then assert Update's publish blocks past an
// instantaneous drop and lands once the reader catches up.
func TestUpdate_TerminalWrite_UsesPublishMustDeliver(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	// Give must-deliver a small but non-zero timeout so the test is
	// fast without being flaky, and so we can distinguish "dropped
	// immediately like Publish" from "waited, like PublishMustDeliver".
	svc.SetMustDeliverTimeout(300 * time.Millisecond)

	ctx := t.Context()
	sessionID := "test-session-mustdeliver"

	// Subscribe before Create so we have a channel to observe/fill;
	// otherwise Create's CreatedEvent is published to no one.
	sub := svc.Subscribe(ctx)

	created, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	// Drain the CreatedEvent (Create uses best-effort Publish) so it
	// doesn't interfere with counting the Update below.
	<-sub

	// Fill the subscriber's buffer completely using best-effort
	// Publish on the embedded broker, so the next publish (Update's)
	// has no free slot and must take PublishMustDeliver's
	// bounded-blocking path.
	bufCap := cap(sub)
	for range bufCap {
		svc.Publish(pubsub.CreatedEvent, created)
	}

	// Now call Update concurrently. Because the buffer is full,
	// PublishMustDeliver must take its bounded-blocking slow path
	// instead of dropping instantly.
	created.Parts = append(created.Parts, Finish{Reason: "stop", Time: time.Now().Unix()})
	updateDone := make(chan error, 1)
	start := time.Now()
	go func() {
		updateDone <- svc.Update(ctx, created)
	}()

	// Give Update a brief head start to ensure it has entered the
	// blocking path before we start draining, then drain one slot so
	// delivery can succeed within the timeout.
	time.Sleep(50 * time.Millisecond)
	<-sub // free one slot

	select {
	case err := <-updateDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Update did not return")
	}
	elapsed := time.Since(start)

	// If Update had used best-effort Publish, it would have returned
	// (and dropped) almost instantly. Seeing it take a non-trivial
	// amount of time (waiting for the drained slot) instead of an
	// instant no-op is the observable signature of PublishMustDeliver's
	// bounded-blocking path being used, and it must still be well under
	// the outer safety bound.
	assert.Less(t, elapsed, time.Second, "Update should not block indefinitely")

	// Drain the rest of the filler events plus the delivered update
	// looking for the UpdatedEvent carrying our new part.
	found := false
	for i := 0; i < bufCap; i++ {
		select {
		case ev := <-sub:
			if ev.Type == pubsub.UpdatedEvent && len(ev.Payload.Parts) == 2 {
				found = true
			}
		default:
		}
	}
	assert.True(t, found, "expected the Update's UpdatedEvent to have been delivered, not dropped")
}

// TestUpdate_PartialCheckpoint_UsesBestEffortPublish verifies the
// counterpart of TestUpdate_TerminalWrite_UsesPublishMustDeliver: an
// Update whose Finish part is a Partial checkpoint (written by the
// auto-checkpoint ticker every ~2s during streaming) is published via
// best-effort Publish, NOT PublishMustDeliver. With a full subscriber
// buffer it must return near-instantly (dropping the event for the slow
// subscriber) instead of blocking for the configured must-deliver
// timeout — losing one mid-stream tick is harmless because the next
// ticker tick or the terminal Finish re-establishes current state.
func TestUpdate_PartialCheckpoint_UsesBestEffortPublish(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	// A long must-deliver timeout: if Update mistakenly routed the
	// partial checkpoint through PublishMustDeliver, this test would
	// observably stall ~2s against the full buffer. Best-effort Publish
	// returns near-instantly regardless.
	svc.SetMustDeliverTimeout(2 * time.Second)

	ctx := t.Context()
	sessionID := "test-session-partial-checkpoint"

	sub := svc.Subscribe(ctx)

	created, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "streaming"}},
	})
	require.NoError(t, err)

	// Drain the CreatedEvent (Create uses best-effort Publish) so the
	// buffer is empty before we fill it below.
	<-sub

	// A mid-stream checkpoint snapshot: a Finish with Partial==true, as
	// the auto-checkpoint ticker writes it (Reason empty, finished_at
	// stays NULL in the DB row).
	created.Parts = append(created.Parts, Finish{
		Time:    time.Now().Unix(),
		Partial: true,
	})

	// Fill the subscriber buffer completely so the next publish has no
	// free slot: best-effort Publish must drop it, PublishMustDeliver
	// would block ~timeout.
	bufCap := cap(sub)
	for range bufCap {
		svc.Publish(pubsub.CreatedEvent, created)
	}
	dropsBefore := svc.DropCount()

	start := time.Now()
	err = svc.Update(ctx, created)
	elapsed := time.Since(start)
	require.NoError(t, err)

	// Best-effort Publish returns near-instantly despite the full
	// buffer; PublishMustDeliver would have blocked ~2s here.
	assert.Less(t, elapsed, 200*time.Millisecond,
		"Partial checkpoint Update must not block on a full subscriber buffer")

	// The partial snapshot was dropped for the slow subscriber via the
	// best-effort path (DropCount), not the must-deliver path
	// (MustDeliverDropCount must stay zero — no bounded wait occurred).
	assert.Equal(t, dropsBefore+1, svc.DropCount(),
		"Partial checkpoint should be dropped via best-effort Publish on a full buffer")
	assert.Equal(t, uint64(0), svc.MustDeliverDropCount(),
		"Partial checkpoint must not exercise the must-deliver timeout path")
}
