package message

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/db"
)

func newTestMessageDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })

	// Run migrations
	_, err = sqlDB.Exec(`
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
			hidden INTEGER NOT NULL DEFAULT 0
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
