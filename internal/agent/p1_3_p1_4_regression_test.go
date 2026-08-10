package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP1_3_CoordinatorSummarizeSingleRead verifies that coordinator.Summarize
// reads the model ONCE instead of reading it twice (once for provider config,
// once for getProviderOptions). Without this fix (P1-3 from the 2026-08-07
// concurrency review), a concurrent SetModels between the two reads would
// mismatch the provider config from the actually used model.
//
// This test mocks the coordinator's currentAgent and verifies that Model()
// is called exactly once during Summarize, proving the single-read pattern.
//
// REVERT CHECK PROCEDURE:
//  1. In coordinator.go Summarize (lines 2501-2518), replace:
//     agentModel := c.currentAgent.Model()
//     providerCfg, ok := c.cfg.Config().Providers.Get(agentModel.ModelCfg.Provider)
//     ...
//     summarize := func() error {
//     return c.currentAgent.Summarize(ctx, sessionID, getProviderOptions(agentModel, providerCfg))
//     }
//     with the buggy version (two separate reads):
//     providerCfg, ok := c.cfg.Config().Providers.Get(c.currentAgent.Model().ModelCfg.Provider)
//     ...
//     summarize := func() error {
//     return c.currentAgent.Summarize(ctx, sessionID, getProviderOptions(c.currentAgent.Model(), providerCfg))
//     }
//  2. Run: go test ./internal/agent -run TestP1_3_CoordinatorSummarizeSingleRead -v
//  3. The test will FAIL because Model() will be called twice instead of once
//  4. Restore the fix and the test will PASS again.
func TestP1_3_CoordinatorSummarizeSingleRead(t *testing.T) {
	t.Parallel()

	// Create a mock server for summaries.
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{"content":"summary"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	// Build test environment.
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "model-a")
	require.NoError(t, err)
	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg: config.SelectedModel{
			Provider: "openaicompat",
			Model:    "model-a",
		},
	}

	env := testEnv(t)

	// Create a spy agent that counts Model() calls.
	spyAgent := &modelCallSpyAgent{
		model:    model,
		sessions: env.sessions,
		messages: env.messages,
	}

	// Register the provider in config (create config first).
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "model-a", Name: "model-a", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	// NewCoordinator's buildAgentModels needs a selected large/small model
	// to construct the coordinator at all — found via a CI-only failure
	// ("large model not selected") that never reproduced locally (some
	// unidentified environment leak on the dev machine apparently made this
	// already true). The actual value here is thrown away immediately below
	// (coord.currentAgent is overwritten with spyAgent), so any provider/
	// model pair that resolves cleanly is fine — reusing the same
	// provider/model this test already built its spy model from.
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeLarge, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "model-a",
	})
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeSmall, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "model-a",
	})

	// Create a coordinator with the spy agent.
	c, err := NewCoordinator(t.Context(), cfg, env.sessions, env.messages, env.permissions, env.history, *env.filetracker, nil)
	require.NoError(t, err)
	coord := c.(*coordinator)
	coord.currentAgent = spyAgent

	// Create a session with enough messages to trigger summarization.
	sess, err := env.sessions.Create(t.Context(), "p1-3 coordinator test")
	require.NoError(t, err)

	// Create 8 messages (4 user + 4 assistant).
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Call Summarize. With the FIX (correct code), Model() should be called ONCE.
	err = c.Summarize(t.Context(), sess.ID, nil)
	require.NoError(t, err)

	// Verify that Model() was called exactly ONCE.
	// Without the P1-3 fix, it would be called twice (once for providerCfg,
	// once for getProviderOptions).
	require.Equal(t, int32(1), spyAgent.modelCallCount.Load(),
		"coordinator.Summarize should call Model() exactly once, got %d calls", spyAgent.modelCallCount.Load())

	// Verify the summary was created.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var summaryMsg *message.Message
	for i := range msgs {
		if msgs[i].IsSummaryMessage {
			summaryMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, summaryMsg, "summary message should exist")
	require.Equal(t, "model-a", summaryMsg.Model, "summary should use model-a")
}

// modelCallSpyAgent is a spy that counts how many times Model() is called.
type modelCallSpyAgent struct {
	model          Model
	modelCallCount atomic.Int32
	sessions       session.Service
	messages       message.Service
}

func (m *modelCallSpyAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (m *modelCallSpyAgent) SetModels(large, small Model) {
	m.model = large
}

func (m *modelCallSpyAgent) SetTools(tools []fantasy.AgentTool)  {}
func (m *modelCallSpyAgent) SetSystemPrompt(systemPrompt string) {}

func (m *modelCallSpyAgent) Model() Model {
	m.modelCallCount.Add(1)
	return m.model
}

func (m *modelCallSpyAgent) Summarize(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	// Mock implementation that creates a summary message without calling a provider.
	// This is fine for testing the coordinator's single-read behavior.
	msg, err := m.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Parts:            []message.ContentPart{message.TextContent{Text: "summary"}},
		Model:            m.model.ModelCfg.Model,
		Provider:         m.model.ModelCfg.Provider,
		IsSummaryMessage: true,
	})
	if err != nil {
		return err
	}
	// Update to mark as summary and set Model field.
	msg.IsSummaryMessage = true
	msg.Model = m.model.ModelCfg.Model
	msg.Provider = m.model.ModelCfg.Provider
	return m.messages.Update(ctx, msg)
}

func (m *modelCallSpyAgent) Cancel(sessionID string) {}

func (m *modelCallSpyAgent) CancelAll() (stillBusy bool) {
	return false
}

func (m *modelCallSpyAgent) IsSessionBusy(sessionID string) bool {
	return false
}

func (m *modelCallSpyAgent) IsBusy() bool {
	return false
}

func (m *modelCallSpyAgent) QueuedPrompts(sessionID string) int {
	return 0
}

func (m *modelCallSpyAgent) QueuedPromptsList(sessionID string) []string {
	return nil
}

func (m *modelCallSpyAgent) ClearQueue(sessionID string) {}

func (m *modelCallSpyAgent) InterruptAndSend(ctx context.Context, sessionID, prompt string, large, small *ModelOverride, attachments ...message.Attachment) error {
	return nil
}

func (m *modelCallSpyAgent) InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	return message.Message{}, nil
}

func (m *modelCallSpyAgent) SummarizeQueued(sessionID string) bool {
	return false
}

func (m *modelCallSpyAgent) TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool) {
	return nil, false
}

func (m *modelCallSpyAgent) CancelQueuedSummarize(sessionID string) {}

func (m *modelCallSpyAgent) UpdateModels(ctx context.Context) error {
	return nil
}

func (m *modelCallSpyAgent) GetSystemPrompt() string {
	return ""
}

func (m *modelCallSpyAgent) BuildSystemPrompt(ctx context.Context) (string, error) {
	return "", nil
}

func (m *modelCallSpyAgent) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return nil
}

func (m *modelCallSpyAgent) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {}

func (m *modelCallSpyAgent) SetRunLimits(maxCost float64, maxTokens int64) {}

func (m *modelCallSpyAgent) SetActiveModelRole(role config.SelectedModelType) {}

func (m *modelCallSpyAgent) SetAllowPeakHours(allow bool) {}

func (m *modelCallSpyAgent) SetPersistentMode(persistent bool) {}

func (m *modelCallSpyAgent) ResetAutoResumeCounter(sessionID string) {}

func (m *modelCallSpyAgent) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (SessionAgentCall, error) {
	return SessionAgentCall{}, nil
}

func (m *modelCallSpyAgent) RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (m *modelCallSpyAgent) SetSystemPromptPrefix(prefix string) {}

func (m *modelCallSpyAgent) SetTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
}

func (m *modelCallSpyAgent) SystemPrompt() string {
	return ""
}

func (m *modelCallSpyAgent) QueueMessage(call SessionAgentCall) {}

func (m *modelCallSpyAgent) InterruptAndReplace(sessionID string, call SessionAgentCall) bool {
	return false
}

func (m *modelCallSpyAgent) SmallModel() Model {
	return m.model
}

func (m *modelCallSpyAgent) LargeModel() Model {
	return m.model
}

// TestP1_4_CleanupUsesCancelImmuneContext verifies that cleanup DB operations
// on summary cancel/error paths use a bounded cancel-immune context instead of
// the already-canceled stream context. Without this, Delete/Update would
// silently fail and leave orphaned summary messages in the DB (P1-4 from the
// 2026-08-07 concurrency review).
//
// The test creates an already-canceled context before calling runSummarizeSilent.
// When the provider call fails with context.Canceled, the cleanup code must use
// a cancel-immune context to delete the orphaned summary message.
//
// REVERT CHECK PROCEDURE:
//  1. In runSummarizeSilent (agent.go:~3404-3415), replace:
//     cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
//     defer cleanupCancel()
//     deleteErr := a.messages.Delete(cleanupCtx, summaryMessage.ID)
//     with:
//     deleteErr := a.messages.Delete(ctx, summaryMessage.ID)  // BUG: Use canceled context
//  2. Run: go test ./internal/agent -run TestP1_4_CleanupUsesCancelImmuneContext -v
//  3. The test will FAIL because the orphaned summary message will NOT be deleted
//     (the List will find an IsSummaryMessage=true message that shouldn't exist)
//  4. Restore the fix and the test will PASS again.
func TestP1_4_CleanupUsesCancelImmuneContext(t *testing.T) {
	t.Parallel()

	// Create a mock provider that blocks so we can cancel the context mid-stream.
	var streamStarted atomic.Bool
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		streamStarted.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Send the role delta.
		fmt.Fprintf(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"}}]}\n\n`)
		fl.Flush()

		// Block forever - the test will cancel the context.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// Build the test environment.
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a summarizer",
		SystemPromptPrefix:   "",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sa := a.(*sessionAgent)

	// Create a session with enough messages to trigger summarization.
	sess, err := env.sessions.Create(context.Background(), "p1-4 cleanup test")
	require.NoError(t, err)

	// Create 8 messages (4 user + 4 assistant).
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Create a cancellable context.
	ctx, cancel := context.WithCancel(context.Background())

	// Start the summary in a goroutine.
	summarizeDone := make(chan error)
	go func() {
		largeModel := sa.largeModel.Get()
		systemPromptPrefix := sa.systemPromptPrefix.Get()
		summarizeDone <- sa.runSummarizeSilent(ctx, sess.ID, fantasy.ProviderOptions{}, largeModel, systemPromptPrefix)
	}()

	// Wait for the stream to start.
	for !streamStarted.Load() {
		select {
		case <-ctx.Done():
			t.Fatal("context canceled before stream started")
		default:
		}
	}

	// Now cancel the context mid-stream.
	cancel()

	// The summary should fail with context.Canceled.
	err = <-summarizeDone
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded),
		"error should be context.Canceled or context.DeadlineExceeded, got: %T", err)

	// Verify that the orphaned summary message was cleaned up (deleted).
	// Without the cancel-immune context fix, Delete would use the already-canceled
	// context and silently fail, leaving the summary message in the DB.
	msgs, err := env.messages.List(context.Background(), sess.ID)
	require.NoError(t, err)

	for _, m := range msgs {
		if m.IsSummaryMessage {
			t.Fatalf("orphaned summary message should have been deleted after context.Canceled, but found: %+v (Model=%s, Provider=%s)",
				m.ID, m.Model, m.Provider)
		}
	}

	// Also verify the provider was actually called (the test setup is valid).
	require.GreaterOrEqual(t, calls.Load(), int64(1), "summary stream should have been invoked")
}
