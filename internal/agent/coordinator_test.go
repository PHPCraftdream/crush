package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionAgent is a minimal mock for the SessionAgent interface.
type mockSessionAgent struct {
	model       Model
	runFunc     func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	cancelled   []string
	queuedCalls []SessionAgentCall
}

func (m *mockSessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return m.runFunc(ctx, call)
}

func (m *mockSessionAgent) Model() Model                        { return m.model }
func (m *mockSessionAgent) SetModels(large, small Model)        {}
func (m *mockSessionAgent) SetTools(tools []fantasy.AgentTool)  {}
func (m *mockSessionAgent) SetSystemPrompt(systemPrompt string) {}
func (m *mockSessionAgent) Cancel(sessionID string) {
	m.cancelled = append(m.cancelled, sessionID)
}
func (m *mockSessionAgent) CancelAll()                                  {}
func (m *mockSessionAgent) IsSessionBusy(sessionID string) bool         { return false }
func (m *mockSessionAgent) IsBusy() bool                                { return false }
func (m *mockSessionAgent) QueuedPrompts(sessionID string) int          { return 0 }
func (m *mockSessionAgent) QueuedPromptsList(sessionID string) []string { return nil }
func (m *mockSessionAgent) ClearQueue(sessionID string)                 {}
func (m *mockSessionAgent) QueueMessage(call SessionAgentCall) {
	m.queuedCalls = append(m.queuedCalls, call)
}

func (m *mockSessionAgent) InjectMessage(_ context.Context, call SessionAgentCall) (message.Message, error) {
	m.queuedCalls = append(m.queuedCalls, call)
	return message.Message{SessionID: call.SessionID}, nil
}

func (m *mockSessionAgent) Summarize(context.Context, string, fantasy.ProviderOptions) error {
	return nil
}
func (m *mockSessionAgent) SummarizeQueued(string) bool { return false }
func (m *mockSessionAgent) TakeSummarizeQueue(string) (fantasy.ProviderOptions, bool) {
	return fantasy.ProviderOptions{}, false
}
func (m *mockSessionAgent) CancelQueuedSummarize(string)          {}
func (m *mockSessionAgent) SetSystemPromptPrefix(string)          {}
func (m *mockSessionAgent) SystemPrompt() string                  { return "" }
func (m *mockSessionAgent) SetTimeoutOptions(bool, time.Duration) {}

// newTestCoordinator creates a minimal coordinator for unit testing runSubAgent.
func newTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, providerCfg)
	return &coordinator{
		cfg:      cfg,
		sessions: env.sessions,
	}
}

// newMockAgent creates a mockSessionAgent with the given provider and run function.
func newMockAgent(providerID string, maxTokens int64, runFunc func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)) *mockSessionAgent {
	return &mockSessionAgent{
		model: Model{
			CatwalkCfg: catwalk.Model{
				DefaultMaxTokens: maxTokens,
			},
			ModelCfg: config.SelectedModel{
				Provider: providerID,
			},
		},
		runFunc: runFunc,
	}
}

// agentResultWithText creates a minimal AgentResult with the given text response.
func agentResultWithText(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: text},
			},
		},
	}
}

func TestRunSubAgent(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("happy path", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			assert.Equal(t, "do something", call.Prompt)
			assert.Equal(t, int64(4096), call.MaxOutputTokens)
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "do something",
			SessionTitle:   "Test Session",
		})
		require.NoError(t, err)
		assert.Equal(t, "done", resp.Content)
		assert.False(t, resp.IsError)
	})

	t.Run("cost update failure preserves output", func(t *testing.T) {
		// A failure to charge the parent session must not discard the
		// sub-agent output that was already produced. Using a parent
		// SessionID that was never created makes IncrementCost fail.
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("output before cost failure"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      "missing-parent-session",
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "output before cost failure", resp.Content)
	})

	t.Run("nil result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("empty result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("ModelCfg.MaxTokens overrides default", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := &mockSessionAgent{
			model: Model{
				CatwalkCfg: catwalk.Model{
					DefaultMaxTokens: 4096,
				},
				ModelCfg: config.SelectedModel{
					Provider:  providerID,
					MaxTokens: 8192,
				},
			},
			runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				assert.Equal(t, int64(8192), call.MaxOutputTokens)
				return agentResultWithText("ok"), nil
			},
		}

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Content)
	})

	t.Run("session creation failure with canceled context", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, nil)

		// Use a canceled context to trigger CreateTaskSession failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = coord.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
	})

	t.Run("provider not configured", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		// Agent references a provider that doesn't exist in config.
		agent := newMockAgent("unknown-provider", 4096, nil)

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "model provider not configured")
	})

	t.Run("agent run error returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("provider request failed")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		// runSubAgent returns (errorResponse, nil) when agent.Run fails — not a Go error.
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Failed to generate response: provider request failed", resp.Content)
	})

	t.Run("session setup callback is invoked", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var setupCalledWith string
		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
			SessionSetup: func(sessionID string) {
				setupCalledWith = sessionID
			},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, setupCalledWith, "SessionSetup should have been called")
	})

	t.Run("cost propagation to parent session", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// Simulate the agent incurring cost on the child session via the
			// race-safe additive API (Save no longer writes the cost column).
			if _, err := env.sessions.IncrementCost(ctx, call.SessionID, 0.05); err != nil {
				return nil, err
			}
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, updated.Cost, 1e-9)
	})
}

func TestUpdateParentSessionCost(t *testing.T) {
	t.Run("accumulates cost correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		// Set child cost via the additive API (Save no longer writes cost).
		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.10)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.10, updated.Cost, 1e-9)
	})

	t.Run("accumulates multiple child costs", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child1, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child1")
		require.NoError(t, err)
		_, err = env.sessions.IncrementCost(t.Context(), child1.ID, 0.05)
		require.NoError(t, err)

		child2, err := env.sessions.CreateTaskSession(t.Context(), "tool-2", parent.ID, "Child2")
		require.NoError(t, err)
		_, err = env.sessions.IncrementCost(t.Context(), child2.ID, 0.03)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child1.ID, parent.ID)
		require.NoError(t, err)
		err = coord.updateParentSessionCost(t.Context(), child2.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.08, updated.Cost, 1e-9)
	})

	t.Run("child session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), "non-existent", parent.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get child session")
	})

	t.Run("parent session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, "non-existent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "increment parent session cost")
	})

	t.Run("zero cost handled correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, updated.Cost, 1e-9)
	})
}

func TestGetProviderOptionsReasoningEffort(t *testing.T) {
	// Bedrock is Fantasy's Anthropic under a different provider name; options
	// must land under anthropic.Name so the Anthropic language model picks them up.
	tests := []struct {
		name         string
		providerType catwalk.Type
	}{
		{"anthropic honors reasoning_effort", catwalk.Type(anthropic.Name)},
		{"bedrock honors reasoning_effort", catwalk.Type(bedrock.Name)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{
					ID:              "claude-opus-4-7",
					CanReason:       true,
					ReasoningLevels: []string{"max"},
				},
				ModelCfg: config.SelectedModel{
					Provider:        "test",
					ReasoningEffort: "max",
				},
			}
			providerCfg := config.ProviderConfig{ID: "test", Type: tc.providerType}

			opts := getProviderOptions(model, providerCfg)

			raw, ok := opts[anthropic.Name]
			require.True(t, ok, "options should be keyed under anthropic.Name for type %q", tc.providerType)
			parsed, ok := raw.(*anthropic.ProviderOptions)
			require.True(t, ok)
			require.NotNil(t, parsed.Effort)
			assert.Equal(t, anthropic.Effort("max"), *parsed.Effort)
		})
	}
}

// Pins the contract of shouldRetryStalledMessage: a watchdog-stalled turn
// is only worth re-running when the assistant message is genuinely empty.
// ANY content reaching the assistant — text, reasoning, even a half-emitted
// tool call — proves the server received and processed the prompt; the
// retry is for cases where nothing came back at all. Prevents the
// duplicate-user-message bug observed in or-coin sessions where z.ai went
// silent at the tail of the stream after a complete reply and the retry
// loop re-sent the same prompt 2× more, copying the user message in the
// DB three times.
func TestShouldRetryStalledMessage(t *testing.T) {
	t.Parallel()

	stalledFinish := message.Finish{
		Reason:  message.FinishReasonError,
		Message: "Stream stalled",
	}

	t.Run("no finish part returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant}
		assert.False(t, shouldRetryStalledMessage(m))
	})

	t.Run("non-stalled finish returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn},
		}}
		assert.False(t, shouldRetryStalledMessage(m))
	})

	t.Run("stalled with no content returns true", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{stalledFinish}}
		assert.True(t, shouldRetryStalledMessage(m), "empty stalled turn must be retried")
	})

	t.Run("stalled with whitespace-only text returns true", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "   \n\t "},
			stalledFinish,
		}}
		assert.True(t, shouldRetryStalledMessage(m), "whitespace-only output is no output")
	})

	t.Run("stalled with any real text returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "ok"},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "any answer means the server saw the prompt")
	})

	t.Run("stalled with reasoning only returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "considering options..."},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "reasoning proves the model started working")
	})

	t.Run("stalled with finished tool call returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "bash", Finished: true},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "a completed tool call counts as real work")
	})

	t.Run("stalled with unfinished tool call returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "bash", Finished: false},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "even a partial tool call proves the prompt was received")
	})
}

// providerErr is a tiny constructor to keep the classify table terse.
func providerErr(status int, msg string) *fantasy.ProviderError {
	return &fantasy.ProviderError{
		StatusCode: status,
		Title:      fantasy.ErrorTitleForStatusCode(status),
		Message:    msg,
	}
}

func TestClassifyProviderError(t *testing.T) {
	t.Parallel()

	quotaMsg := "Usage limit reached for 5 hour. Your limit will reset at 2025-01-01T00:00:00Z"
	overloadMsg := "The service may be temporarily overloaded"

	// status 0 wrapping io.ErrUnexpectedEOF → IsRetryable()==true.
	zeroRetryable := &fantasy.ProviderError{
		StatusCode: 0,
		Message:    io.ErrUnexpectedEOF.Error(),
		Cause:      io.ErrUnexpectedEOF,
	}
	// status 0 with a non-retryable cause → terminal.
	zeroTerminal := &fantasy.ProviderError{StatusCode: 0, Message: "weird"}

	// context-too-large: IsContextTooLarge() reads ContextMaxTokens / ContextTooLargeErr.
	contextTooLarge := &fantasy.ProviderError{StatusCode: 400, ContextMaxTokens: 200000}

	tests := []struct {
		name string
		err  error
		want retryClass
	}{
		{"context.Canceled", context.Canceled, classTerminal},
		{"context.DeadlineExceeded", context.DeadlineExceeded, classTerminal},

		{"401", providerErr(http.StatusUnauthorized, "nope"), classTerminal},
		{"402", providerErr(http.StatusPaymentRequired, "pay"), classTerminal},
		{"403", providerErr(http.StatusForbidden, "forbidden"), classTerminal},

		{"429 quota wall", providerErr(http.StatusTooManyRequests, quotaMsg), classTerminal},
		{"429 overload", providerErr(http.StatusTooManyRequests, overloadMsg), classTransient},

		{"408", providerErr(http.StatusRequestTimeout, "timeout"), classTransient},
		{"409", providerErr(http.StatusConflict, "conflict"), classTransient},

		{"500", providerErr(http.StatusInternalServerError, "boom"), classTransient},
		{"503", providerErr(http.StatusServiceUnavailable, "down"), classTransient},

		{"400", providerErr(http.StatusBadRequest, "bad"), classTerminal},
		{"404", providerErr(http.StatusNotFound, "missing"), classTerminal},

		{"status 0 EOF retryable", zeroRetryable, classTransient},
		{"status 0 non-retryable", zeroTerminal, classTerminal},

		{"context-too-large", contextTooLarge, classTerminal},

		{"plain net.OpError (no ProviderError)", &net.OpError{Op: "read", Err: errors.New("connection reset")}, classTransient},
		{"plain generic error", errors.New("something else"), classTerminal},

		// RetryError wrapping must be transparent to errors.As.
		{
			"RetryError wrapping 429 overload",
			&fantasy.RetryError{Errors: []error{providerErr(http.StatusTooManyRequests, overloadMsg)}},
			classTransient,
		},
		{
			"RetryError wrapping 429 quota",
			&fantasy.RetryError{Errors: []error{providerErr(http.StatusTooManyRequests, quotaMsg)}},
			classTerminal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyProviderError(tc.err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestIsQuotaLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *fantasy.ProviderError
		want bool
	}{
		{"5h usage limit", providerErr(http.StatusTooManyRequests, "Usage limit reached for 5 hour. Your limit will reset at 2025-01-01T00:00:00Z"), true},
		{"reset at", providerErr(http.StatusTooManyRequests, "Rate limit reset at epoch 1234"), true},
		{"quota", providerErr(http.StatusTooManyRequests, "You exceeded your quota"), true},
		{"overload", providerErr(http.StatusTooManyRequests, "The service may be temporarily overloaded"), false},
		{"empty message", providerErr(http.StatusTooManyRequests, ""), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isQuotaLimit(tc.err))
		})
	}
}

func TestTurnMadeProgress(t *testing.T) {
	t.Parallel()

	t.Run("empty message is no progress", func(t *testing.T) {
		t.Parallel()
		assert.False(t, turnMadeProgress(message.Message{Role: message.Assistant}))
	})
	t.Run("whitespace only is no progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "  \n\t "},
		}}
		assert.False(t, turnMadeProgress(m))
	})
	t.Run("text is progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		}}
		assert.True(t, turnMadeProgress(m))
	})
	t.Run("reasoning is progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "thinking..."},
		}}
		assert.True(t, turnMadeProgress(m))
	})
	t.Run("tool call is progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "bash"},
		}}
		assert.True(t, turnMadeProgress(m))
	})
}

// appendAssistant finishes a fresh assistant message in the session with
// the given parts, returning the coordinator bound to the test's message
// service so shouldRetryTurn can read it back.
func appendAssistant(t *testing.T, env fakeEnv, parts []message.ContentPart) (*coordinator, string) {
	t.Helper()
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}

	sess, err := env.sessions.Create(t.Context(), "retry-test")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: parts,
	})
	require.NoError(t, err)
	return coord, sess.ID
}

func TestShouldRetryTurn(t *testing.T) {
	emptyStreamFinish := message.Finish{Reason: message.FinishReasonError, Message: "Empty response"}
	stallFinish := message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle}
	cleanFinish := message.Finish{Reason: message.FinishReasonEndTurn}

	overloadErr := providerErr(http.StatusTooManyRequests, "The service may be temporarily overloaded")
	quotaErr := providerErr(http.StatusTooManyRequests, "Your quota has been exhausted")

	t.Run("stall title with nil error retries", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{stallFinish})
		assert.True(t, coord.shouldRetryTurn(t.Context(), sid, context.Canceled))
	})

	t.Run("empty-stream finish with nil error retries", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{emptyStreamFinish})
		assert.True(t, coord.shouldRetryTurn(t.Context(), sid, nil))
	})

	t.Run("429 overload error retries", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{emptyStreamFinish})
		assert.True(t, coord.shouldRetryTurn(t.Context(), sid, overloadErr))
	})

	t.Run("429 quota error does not retry", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{emptyStreamFinish})
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, quotaErr))
	})

	t.Run("turn with content does not retry even on transient error", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "partial answer"},
			emptyStreamFinish,
		})
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, overloadErr))
	})

	t.Run("clean end_turn finish does not retry", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{cleanFinish})
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, nil))
	})

	t.Run("no assistant message does not retry", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}
		sess, err := env.sessions.Create(t.Context(), "empty")
		require.NoError(t, err)
		assert.False(t, coord.shouldRetryTurn(t.Context(), sess.ID, overloadErr))
	})
}

func TestBackgroundJobSummary(t *testing.T) {
	t.Parallel()

	t.Run("with stdout", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00A", "echo hi && make build", "hello world", "", 0, 42*time.Second)
		assert.Contains(t, got, "00A")
		assert.Contains(t, got, "`echo hi && make build`")
		assert.Contains(t, got, "exit 0")
		assert.Contains(t, got, "42s")
		assert.Contains(t, got, "hello world")
	})

	t.Run("exit code and stderr surfaced", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00B", "make test", "", "boom: tests failed", 2, 90*time.Second)
		assert.Contains(t, got, "exit 2")
		assert.Contains(t, got, "1m30s")
		assert.Contains(t, got, "boom: tests failed")
	})

	t.Run("no output falls back to placeholder", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00C", "true", "  \n ", "", 0, 3*time.Second)
		assert.Contains(t, got, "(no output)")
	})

	t.Run("both stdout and stderr are joined", func(t *testing.T) {
		t.Parallel()
		got := backgroundJobSummary("00D", "go test ./...", "ok pkg 0.1s", "warn: deprecated", 0, 5*time.Second)
		assert.Contains(t, got, "ok pkg 0.1s")
		assert.Contains(t, got, "warn: deprecated")
	})
}
