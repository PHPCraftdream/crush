package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
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

// boolPtr is a tiny helper for building *bool config values in tests.
func boolPtr(b bool) *bool { return &b }

func TestConsecutiveAutoResumeCounter(t *testing.T) {
	coord := &coordinator{consecutiveAutoResumes: make(map[string]int)}

	t.Run("starts at zero", func(t *testing.T) {
		assert.Equal(t, 0, coord.consecutiveResume("sess-1"))
	})

	t.Run("bump increments and consecutiveResume reflects it", func(t *testing.T) {
		coord.bumpConsecutiveResume("sess-1")
		coord.bumpConsecutiveResume("sess-1")
		assert.Equal(t, 2, coord.consecutiveResume("sess-1"))
	})

	t.Run("reset clears to zero", func(t *testing.T) {
		coord.bumpConsecutiveResume("sess-reset")
		require.Equal(t, 1, coord.consecutiveResume("sess-reset"))
		coord.resetConsecutiveResume("sess-reset")
		assert.Equal(t, 0, coord.consecutiveResume("sess-reset"))
	})

	t.Run("sessions are independent", func(t *testing.T) {
		coord.bumpConsecutiveResume("a")
		coord.bumpConsecutiveResume("a")
		coord.bumpConsecutiveResume("b")
		assert.Equal(t, 2, coord.consecutiveResume("a"))
		assert.Equal(t, 1, coord.consecutiveResume("b"))
	})

	t.Run("reset on unknown session is a no-op", func(t *testing.T) {
		coord.resetConsecutiveResume("never-seen")
		assert.Equal(t, 0, coord.consecutiveResume("never-seen"))
	})

	t.Run("concurrent bumps are serialized by the mutex", func(t *testing.T) {
		const sessionID = "sess-concurrent"
		const n = 100
		var wg sync.WaitGroup
		wg.Add(n)
		for range n {
			go func() {
				defer wg.Done()
				coord.bumpConsecutiveResume(sessionID)
			}()
		}
		wg.Wait()
		assert.Equal(t, n, coord.consecutiveResume(sessionID))
	})
}

func TestMaxConsecutiveAutoResumesCap(t *testing.T) {
	// Guard against accidental edits to the runaway bound.
	assert.Equal(t, 5, maxConsecutiveAutoResumes)
}

func TestAutonomyEnabled(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg}

	t.Run("nil Options.AutoResumeOnJobDone defaults disabled", func(t *testing.T) {
		cfg.Config().Options = nil
		assert.False(t, coord.autonomyEnabled())
	})

	t.Run("explicit false stays disabled", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(false)}
		assert.False(t, coord.autonomyEnabled())
	})

	t.Run("explicit true enables", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		assert.True(t, coord.autonomyEnabled())
	})
}

func TestSetPersistentMode(t *testing.T) {
	coord := &coordinator{}
	assert.False(t, coord.persistentMode, "default must be false (crush run is non-persistent)")
	coord.SetPersistentMode(true)
	assert.True(t, coord.persistentMode)
	coord.SetPersistentMode(false)
	assert.False(t, coord.persistentMode)
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

// Ported from upstream f75435a2: when a reasoning-capable model has no
// default_reasoning_effort configured and the user hasn't selected one,
// getProviderOptions must fall back to the first configured reasoning
// level instead of silently disabling reasoning. Uses the openai branch,
// the same path f75435a2 itself exercises upstream — the fork's own
// ZAI/GLM openaicompat mapping (a fork-owned block, not part of this
// upstream commit) has its own analogous default-on behavior, covered
// separately below.
func TestGetProviderOptionsReasoningEffortFallback(t *testing.T) {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ID:              "gpt-5-test",
			CanReason:       true,
			ReasoningLevels: []string{"high", "max"},
		},
		ModelCfg: config.SelectedModel{
			Provider: "openai",
		},
	}
	providerCfg := config.ProviderConfig{ID: "openai", Type: openai.Name}

	opts := getProviderOptions(model, providerCfg)

	raw, ok := opts[openai.Name]
	require.True(t, ok)
	parsed, ok := raw.(*openai.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))
}

// The fork's ZAI/GLM mapping (getProviderOptions, InferenceProviderZAI case)
// has no "unset" state on z.ai's own API — thinking is either on at some
// level or off. An unset ReasoningEffort now defaults thinking ON at "high"
// (z.ai recommends max/high for coding tasks) instead of silently disabling
// reasoning; "off" is the explicit opt-out.
func TestGetProviderOptionsZAIReasoningDefault(t *testing.T) {
	newModel := func(effort string) Model {
		return Model{
			CatwalkCfg: catwalk.Model{
				ID:              "glm-5.2",
				CanReason:       true,
				ReasoningLevels: []string{"high", "max"},
			},
			ModelCfg: config.SelectedModel{
				Provider:        "zai",
				ReasoningEffort: effort,
			},
		}
	}
	providerCfg := config.ProviderConfig{ID: string(catwalk.InferenceProviderZAI), Type: openaicompat.Name}

	extraBody := func(t *testing.T, effort string) map[string]any {
		t.Helper()
		opts := getProviderOptions(newModel(effort), providerCfg)
		raw, ok := opts[openaicompat.Name]
		require.True(t, ok)
		parsed, ok := raw.(*openaicompat.ProviderOptions)
		require.True(t, ok)
		return parsed.ExtraBody
	}

	t.Run("unset defaults to thinking enabled at high", func(t *testing.T) {
		eb := extraBody(t, "")
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "high", eb["reasoning_effort"])
	})

	t.Run("xhigh maps to max", func(t *testing.T) {
		eb := extraBody(t, "xhigh")
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "max", eb["reasoning_effort"])
	})

	t.Run("off explicitly disables thinking", func(t *testing.T) {
		eb := extraBody(t, "off")
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "disabled", thinking["type"])
		_, hasEffort := eb["reasoning_effort"]
		assert.False(t, hasEffort, "reasoning_effort should not be set when thinking is off")
	})
}

// DeepSeek must keep the fork's ORIGINAL default: an unset ReasoningEffort
// leaves thinking OFF. The ZAI-only "unset → thinking on at high" default
// (added in 28ec4145) deliberately does not apply to DeepSeek — this test
// pins that separation so the two providers can't drift back together.
func TestGetProviderOptionsDeepSeekReasoningDefault(t *testing.T) {
	newModel := func(effort string, think bool) Model {
		return Model{
			CatwalkCfg: catwalk.Model{
				ID:        "deepseek-reasoner",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{
				Provider:        "deepseek",
				ReasoningEffort: effort,
				Think:           think,
			},
		}
	}
	providerCfg := config.ProviderConfig{ID: string(catwalk.InferenceProviderDeepSeek), Type: openaicompat.Name}

	extraBody := func(t *testing.T, effort string, think bool) map[string]any {
		t.Helper()
		opts := getProviderOptions(newModel(effort, think), providerCfg)
		raw, ok := opts[openaicompat.Name]
		require.True(t, ok)
		parsed, ok := raw.(*openaicompat.ProviderOptions)
		require.True(t, ok)
		return parsed.ExtraBody
	}

	t.Run("unset leaves thinking disabled (old behavior)", func(t *testing.T) {
		eb := extraBody(t, "", false)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "disabled", thinking["type"])
		_, hasEffort := eb["reasoning_effort"]
		assert.False(t, hasEffort, "reasoning_effort must not be set when effort is unset")
	})

	t.Run("Think enables thinking without explicit effort", func(t *testing.T) {
		eb := extraBody(t, "", true)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		_, hasEffort := eb["reasoning_effort"]
		assert.False(t, hasEffort, "reasoning_effort stays unset when only Think is set")
	})

	t.Run("explicit effort enables thinking and maps high", func(t *testing.T) {
		eb := extraBody(t, "high", false)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "high", eb["reasoning_effort"])
	})

	t.Run("xhigh maps to max", func(t *testing.T) {
		eb := extraBody(t, "xhigh", false)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "max", eb["reasoning_effort"])
	})
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
		{"403", providerErr(http.StatusForbidden, "forbidden"), classTransient},

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

// TestHandleInterruptTick exercises the interrupt-inject tick handler in
// isolation (no live provider): it seeds an interrupt=true pending_injects row
// referencing an already-persisted user message, then asserts the handler
// consumes the row, queues a call that points at the EXISTING message (no
// duplicate create), and cancels the running turn. A second tick with no
// interrupt row must be a no-op.
func TestHandleInterruptTick(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{ID: providerID})

	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("ok"), nil
	})
	coord := &coordinator{
		cfg:          cfg,
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: agent,
	}

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "interrupt-tick")
	require.NoError(t, err)

	// The CLI (`crush sessions inject --interrupt`) creates the user message
	// AND the interrupt row; simulate both here.
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "stop and do X"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID, MessageID: msg.ID, Content: "stop and do X", Interrupt: true,
	}))

	t.Run("fires on interrupt row: queues existing message + cancels", func(t *testing.T) {
		fired, err := coord.handleInterruptTick(ctx, sess.ID)
		require.NoError(t, err)
		assert.True(t, fired)

		require.Len(t, agent.queuedCalls, 1)
		q := agent.queuedCalls[0]
		assert.Equal(t, msg.ID, q.ExistingMessageID, "must reference existing message, not create a new one")
		assert.Equal(t, "stop and do X", q.Prompt)
		require.Len(t, agent.cancelled, 1)
		assert.Equal(t, sess.ID, agent.cancelled[0])

		// No new user message row was created — history still holds exactly
		// the one the CLI created.
		msgs, err := env.messages.List(ctx, sess.ID)
		require.NoError(t, err)
		userCount := 0
		for _, m := range msgs {
			if m.Role == message.User {
				userCount++
			}
		}
		assert.Equal(t, 1, userCount, "no duplicate user message in history")
	})

	t.Run("no interrupt row is a no-op", func(t *testing.T) {
		fired, err := coord.handleInterruptTick(ctx, sess.ID)
		require.NoError(t, err)
		assert.False(t, fired)
		// No additional queue/cancel activity.
		assert.Len(t, agent.queuedCalls, 1)
		assert.Len(t, agent.cancelled, 1)
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

func TestAutoResumeEligible(t *testing.T) {
	// Truth table for the Phase 4 autonomy policy surface. The eligibility
	// decision is the whole gate; the branch in notifyBackgroundJobDone just
	// routes eligible->Run vs not->InjectMessage.
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, consecutiveAutoResumes: make(map[string]int)}
	const sid = "sess-eligible"

	t.Run("autonomy OFF (nil Options) is never eligible regardless of persistentMode", func(t *testing.T) {
		cfg.Config().Options = nil
		coord.persistentMode = true
		assert.False(t, coord.autoResumeEligible(sid))
	})

	t.Run("autonomy OFF (explicit false) is never eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(false)}
		coord.persistentMode = true
		assert.False(t, coord.autoResumeEligible(sid))
	})

	t.Run("autonomy ON + persistentMode false (crush run) is not eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		coord.persistentMode = false
		assert.False(t, coord.autoResumeEligible(sid))
	})

	t.Run("autonomy ON + persistentMode true + counter below cap is eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		coord.persistentMode = true
		coord.resetConsecutiveResume(sid)
		assert.True(t, coord.autoResumeEligible(sid))
	})

	t.Run("at the cap (== maxConsecutiveAutoResumes) flips to not eligible", func(t *testing.T) {
		cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
		coord.persistentMode = true
		coord.resetConsecutiveResume(sid)
		// Bump to exactly the cap; one below the cap is still eligible.
		for i := 0; i < maxConsecutiveAutoResumes-1; i++ {
			coord.bumpConsecutiveResume(sid)
		}
		assert.True(t, coord.autoResumeEligible(sid), "one below the cap must still be eligible")
		// The boundary bump that reaches the cap flips eligibility off.
		coord.bumpConsecutiveResume(sid)
		assert.False(t, coord.autoResumeEligible(sid), "at the cap autonomy must stop")
	})
}

func TestResetAutoResumeCounter(t *testing.T) {
	// The exported wrapper is what the server package calls on the human send
	// path; it must clear the consecutive bound so a human message re-arms
	// autonomy.
	coord := &coordinator{consecutiveAutoResumes: make(map[string]int)}
	const sid = "sess-reset-exported"

	coord.bumpConsecutiveResume(sid)
	coord.bumpConsecutiveResume(sid)
	require.Equal(t, 2, coord.consecutiveResume(sid))

	coord.ResetAutoResumeCounter(sid)
	assert.Equal(t, 0, coord.consecutiveResume(sid))
}

// newRoleModelTestCoordinator builds a coordinator wired with distinct Large,
// Small, and (optionally) Worker model slots, each backed by its own
// offline-safe openai-type provider (building an openai.Provider only
// constructs a client, it never makes a network call, so this is safe to run
// without a real API key/network — see buildOpenaiProvider). Used by
// TestBuildAgentModels_WorkerPreference to exercise buildAgentModels' new
// Worker-substitution branch end to end.
func newRoleModelTestCoordinator(t *testing.T, env fakeEnv, includeWorker bool) *coordinator {
	t.Helper()
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: openai.Name,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}

	cfg.Config().Models[config.SelectedModelTypeLarge] = registerProvider("large-provider", "large-model")
	cfg.Config().Models[config.SelectedModelTypeSmall] = registerProvider("small-provider", "small-model")
	if includeWorker {
		cfg.Config().Models[config.SelectedModelTypeWorker] = registerProvider("worker-provider", "worker-model")
	}

	return &coordinator{
		cfg:      cfg,
		sessions: env.sessions,
	}
}

// TestBuildAgentModels_WorkerPreference pins the "prefer Worker for
// sub-agents when parent is Smart" behavior added alongside
// SetActiveModelRole: buildAgentModels must swap in the Worker model config
// as the sub-agent's large slot only when (a) this is a sub-agent build, (b)
// the active role is unset/"large" (parent running smart, or unknown which
// is treated as smart), and (c) a Worker model is actually configured. Every
// other combination must fall through to today's behavior (Large for
// everything) unchanged.
func TestBuildAgentModels_WorkerPreference(t *testing.T) {
	t.Run("worker configured + isSubAgent + role unset uses Worker for large slot", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, true)
		// activeModelRole left at zero value ("") deliberately: unset must be
		// treated the same as "large" (smart).

		large, small, err := coord.buildAgentModels(t.Context(), true)
		require.NoError(t, err)
		assert.Equal(t, "worker-provider", large.ModelCfg.Provider, "sub-agent large slot must come from Worker")
		assert.Equal(t, "worker-model", large.ModelCfg.Model)
		assert.Equal(t, "small-provider", small.ModelCfg.Provider, "small slot must be unaffected")
	})

	t.Run("worker configured + isSubAgent + role explicitly large uses Worker for large slot", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, true)
		coord.SetActiveModelRole(config.SelectedModelTypeLarge)

		large, _, err := coord.buildAgentModels(t.Context(), true)
		require.NoError(t, err)
		assert.Equal(t, "worker-provider", large.ModelCfg.Provider)
	})

	t.Run("worker NOT configured + isSubAgent falls back to Large (backward compat)", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, false)

		large, _, err := coord.buildAgentModels(t.Context(), true)
		require.NoError(t, err)
		assert.Equal(t, "large-provider", large.ModelCfg.Provider, "must fall back to Large when Worker isn't configured")
		assert.Equal(t, "large-model", large.ModelCfg.Model)
	})

	for _, role := range []config.SelectedModelType{
		config.SelectedModelTypeSmall,
		config.SelectedModelTypeWorker,
		config.SelectedModelTypeReviewer,
	} {
		t.Run("worker configured + isSubAgent + active role "+string(role)+" does not force Worker", func(t *testing.T) {
			env := testEnv(t)
			coord := newRoleModelTestCoordinator(t, env, true)
			coord.SetActiveModelRole(role)

			large, _, err := coord.buildAgentModels(t.Context(), true)
			require.NoError(t, err)
			assert.Equal(t, "large-provider", large.ModelCfg.Provider, "an explicit non-large role for the whole run must not be second-guessed for sub-agents")
		})
	}

	t.Run("top-level agent (isSubAgent=false) always uses Large regardless of Worker config or active role", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, true)
		coord.SetActiveModelRole(config.SelectedModelTypeLarge)

		large, _, err := coord.buildAgentModels(t.Context(), false)
		require.NoError(t, err)
		assert.Equal(t, "large-provider", large.ModelCfg.Provider)
		assert.Equal(t, "large-model", large.ModelCfg.Model)
	})
}

// newWorkerToolTestCoordinator builds a coordinator with distinct Large,
// Small, and (optionally) Worker model slots plus every service buildTools
// needs to actually construct tool instances (permissions/history/
// filetracker/messages) — a superset of newRoleModelTestCoordinator (which
// only wires the model slots, sufficient for buildAgentModels but not
// buildTools) and TestBuildTools_CoderHasAskQuestion's inline fixture.
func newWorkerToolTestCoordinator(t *testing.T, env fakeEnv, includeWorker bool) *coordinator {
	t.Helper()
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: openai.Name,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}

	cfg.Config().Models[config.SelectedModelTypeLarge] = registerProvider("large-provider", "large-model")
	cfg.Config().Models[config.SelectedModelTypeSmall] = registerProvider("small-provider", "small-model")
	if includeWorker {
		cfg.Config().Models[config.SelectedModelTypeWorker] = registerProvider("worker-provider", "worker-model")
	}

	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}
}

// buildSubAgentToolNames runs buildTools for the AgentTask sub-agent config
// and returns the resulting tool names, for assertions in
// TestBuildTools_WorkerToolset below.
func buildSubAgentToolNames(t *testing.T, coord *coordinator) []string {
	t.Helper()
	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "task agent must be configured")

	built, err := coord.buildTools(t.Context(), taskCfg, true)
	require.NoError(t, err)

	names := make([]string, 0, len(built))
	for _, tool := range built {
		names = append(names, tool.Info().Name)
	}
	return names
}

// TestBuildTools_WorkerToolset is the BUG-2 fix's regression + behavior
// suite: the AgentTask sub-agent (spawned by the "agent" tool) must stay
// read-only in every case except when it is genuinely acting as a worker
// (Worker model configured AND the parent run's active role is smart/large
// or unset). Getting this wrong in either direction is bad: granting
// edit/write/bash unconditionally would let a plain search-and-context
// sub-agent mutate the filesystem in the ordinary interactive TUI/web path;
// never granting them makes the whole "smart orchestrator delegates
// hands-on work to a cheap Worker model" feature (see
// docs/plans/2026-07-26-orchestrator-worker-e2e.md, BUG-2) impossible.
func TestBuildTools_WorkerToolset(t *testing.T) {
	workerOnlyTools := []string{"edit", "multiedit", "write", "bash"}
	readOnlyTools := []string{"glob", "grep", "ls", "sourcegraph", "view"}

	t.Run("worker NOT configured, sub-agent stays exactly read-only (backward compat)", func(t *testing.T) {
		env := testEnv(t)
		coord := newWorkerToolTestCoordinator(t, env, false)

		names := buildSubAgentToolNames(t, coord)

		for _, name := range workerOnlyTools {
			assert.NotContains(t, names, name, "worker tool %q must be absent when no Worker model is configured", name)
		}
		for _, name := range readOnlyTools {
			assert.Contains(t, names, name, "read-only tool %q must still be present", name)
		}
		assert.NotContains(t, names, AgentToolName, "sub-agent must never get the agent tool")
	})

	activeSmartRoles := []config.SelectedModelType{"", config.SelectedModelTypeLarge}
	for _, role := range activeSmartRoles {
		t.Run("worker configured + active role "+string(role)+" (unset-or-large), sub-agent gets worker toolset", func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, true)
			if role != "" {
				coord.SetActiveModelRole(role)
			}
			// role == "" left unset deliberately: unset must be treated the
			// same as "large" (smart), mirroring buildAgentModels semantics.

			names := buildSubAgentToolNames(t, coord)

			for _, name := range workerOnlyTools {
				assert.Contains(t, names, name, "worker tool %q must be present when Worker is configured and role is smart", name)
			}
			for _, name := range readOnlyTools {
				assert.Contains(t, names, name, "read-only tool %q must still be present for the worker", name)
			}
			assert.NotContains(t, names, AgentToolName, "worker must not get the agent tool: recursion guard against sub-workers spawning sub-workers")
			assert.NotContains(t, names, tools.AskQuestionToolName, "worker must not get ask_question yet: runSubAgent's error path doesn't frame it as a question round-trip (see resolveReadOnlyTools comment in internal/config/config.go)")
		})
	}

	nonSmartRoles := []config.SelectedModelType{
		config.SelectedModelTypeSmall,
		config.SelectedModelTypeWorker,
		config.SelectedModelTypeReviewer,
	}
	for _, role := range nonSmartRoles {
		t.Run("worker configured but active role "+string(role)+" falls back to read-only", func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, true)
			coord.SetActiveModelRole(role)

			names := buildSubAgentToolNames(t, coord)

			for _, name := range workerOnlyTools {
				assert.NotContains(t, names, name, "worker tool %q must be absent when the operator explicitly chose a non-smart role for the whole run", name)
			}
			for _, name := range readOnlyTools {
				assert.Contains(t, names, name, "read-only tool %q must still be present", name)
			}
		})
	}

	t.Run("top-level coder agent is unaffected by Worker config or active role", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			includeWorker bool
			role          config.SelectedModelType
		}{
			{"no worker, role unset", false, ""},
			{"worker configured, role large", true, config.SelectedModelTypeLarge},
			{"worker configured, role unset", true, ""},
			{"worker configured, role worker", true, config.SelectedModelTypeWorker},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env := testEnv(t)
				coord := newWorkerToolTestCoordinator(t, env, tc.includeWorker)
				if tc.role != "" {
					coord.SetActiveModelRole(tc.role)
				}

				coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
				require.True(t, ok, "coder agent must be configured")

				built, err := coord.buildTools(t.Context(), coderCfg, false)
				require.NoError(t, err)

				names := make([]string, 0, len(built))
				for _, tool := range built {
					names = append(names, tool.Info().Name)
				}
				for _, name := range workerOnlyTools {
					assert.Contains(t, names, name, "coder already has %q unconditionally; worker logic must not remove it", name)
				}
				for _, name := range readOnlyTools {
					assert.Contains(t, names, name, "coder already has %q unconditionally", name)
				}
			})
		}
	})
}

// TestBuildToolsAgentConfig_UnconditionalApplicationWouldBreakBackwardCompat
// proves that regression guard (a) in TestBuildTools_WorkerToolset actually
// guards something: if buildToolsAgentConfig's gate were removed (i.e. the
// worker toolset applied unconditionally to every sub-agent build, worker
// configured or not), the read-only backward-compat case would fail. We
// simulate "unconditional" by calling the config-mutation helper directly
// with a coordinator that satisfies isSubAgent but deliberately has no
// Worker model configured and no active role set -- i.e. exactly the
// backward-compat scenario -- and confirm workerSubAgentActive (the gate)
// correctly reports false, which is what keeps buildToolsAgentConfig from
// mutating AllowedTools in that case. This documents, executably, why the
// gate in buildToolsAgentConfig cannot be dropped.
func TestBuildToolsAgentConfig_UnconditionalApplicationWouldBreakBackwardCompat(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false) // no Worker configured

	require.False(t, coord.workerSubAgentActive(),
		"backward-compat scenario (no Worker configured) must not read as worker-active")

	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok)
	original := append([]string(nil), taskCfg.AllowedTools...)

	// The gated call: must be a no-op copy of taskCfg.
	gated := coord.buildToolsAgentConfig(taskCfg, true)
	assert.Equal(t, original, gated.AllowedTools, "gated call must leave AllowedTools untouched when Worker isn't configured")

	// The unconditional variant this test guards against: manually apply the
	// worker toolset the way buildToolsAgentConfig would if it had no gate at
	// all. If this is what shipped, the regression test above would fail
	// because edit/write/bash would leak into the interactive, no-worker,
	// read-only sub-agent.
	unconditional := append([]string(nil), taskCfg.AllowedTools...)
	unconditional = append(unconditional, workerToolNames...)
	assert.NotEqual(t, original, unconditional,
		"sanity check: applying the worker toolset unconditionally would visibly change AllowedTools, proving the gate is load-bearing")
	for _, name := range workerToolNames {
		assert.Contains(t, unconditional, name)
	}
}

// TestBuildTools_CoderHasAskQuestion is a regression test for a wiring bug
// where tools.NewAskQuestionTool() was constructed in buildTools but
// "ask_question" was never added to allToolNames(), so the AllowedTools
// filter in buildTools silently dropped it for every agent (including the
// top-level coder). The tool object existed and its own unit tests passed,
// and the exit_reason "awaiting_answer" plumbing tested fine in isolation,
// but the two were never wired together end to end — the model could never
// see the tool. This test goes through the real buildTools/AllowedTools
// path (unlike ask_question_test.go, which only constructs the tool
// directly) so it fails if the wiring regresses again.
func TestBuildTools_CoderHasAskQuestion(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: openai.Name,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}
	cfg.Config().Models[config.SelectedModelTypeLarge] = registerProvider("large-provider", "large-model")
	cfg.Config().Models[config.SelectedModelTypeSmall] = registerProvider("small-provider", "small-model")

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}

	coderCfg, ok := cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	require.Contains(t, coderCfg.AllowedTools, tools.AskQuestionToolName,
		"allToolNames() must include ask_question or the coder agent will never be allowed to use it")

	built, err := coord.buildTools(t.Context(), coderCfg, false)
	require.NoError(t, err)

	names := make([]string, 0, len(built))
	for _, tool := range built {
		names = append(names, tool.Info().Name)
	}
	assert.Contains(t, names, tools.AskQuestionToolName,
		"buildTools must return ask_question for the top-level coder agent, not silently drop it in the AllowedTools filter")
}

// TestAllToolNames_CoversUnconditionallyBuiltTools is a guard against this
// entire class of bug in the future: buildTools constructs a fixed slice of
// tools unconditionally (everything except the "agent" and "agentic_fetch"
// tools, which are gated on AllowedTools before construction), and then
// filters ALL of allTools through slices.Contains(agent.AllowedTools, ...).
// If a tool is added to that unconditional-construction list in buildTools
// but its name is never added to allToolNames() (internal/config/config.go),
// it is built and then silently discarded for every agent, exactly like
// ask_question was. This test enumerates the same set of tool names
// buildTools unconditionally constructs and asserts each one is present in
// the coder agent's resolved AllowedTools, which -- with no DisabledTools
// configured -- is exactly allToolNames() (see
// resolveAllowedTools/SetupAgents in internal/config/config.go). We go
// through Agents[AgentCoder].AllowedTools rather than calling allToolNames()
// directly because that function is unexported to internal/config.
func TestAllToolNames_CoversUnconditionallyBuiltTools(t *testing.T) {
	// Mirrors the unconditional append(...) block in coordinator.go's
	// buildTools (currently lines ~1358-1377) that runs regardless of
	// agent.AllowedTools. Keep in sync with that block: if a tool is added
	// there, add its name here too, and this test will catch the case
	// where allToolNames() itself wasn't updated to match.
	unconditionallyBuilt := []string{
		tools.AskQuestionToolName,
		"bash",
		"crush_info",
		"crush_logs",
		"job_output",
		"job_kill",
		"download",
		"edit",
		"multiedit",
		"fetch",
		"glob",
		"grep",
		"ls",
		"sourcegraph",
		"todos",
		"view",
		"write",
	}

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coderCfg, ok := cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	require.Empty(t, cfg.Config().Options.DisabledTools,
		"test assumes no DisabledTools so AllowedTools == allToolNames() verbatim")

	for _, name := range unconditionallyBuilt {
		assert.Contains(t, coderCfg.AllowedTools, name,
			"tool %q is unconditionally constructed by buildTools but missing from allToolNames(); it will be silently dropped by the AllowedTools filter for every agent", name)
	}
}
