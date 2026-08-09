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

// TestP1_3_SummaryUsesImmutableSnapshot verifies that summary paths use an
// immutable snapshot of model/prefix config instead of rereading shared state
// mid-stream. Without this, a concurrent SetModels between snapshot and actual
// use would mismatch provider options from the actually used model (P1-3 from
// the 2026-08-07 concurrency review).
//
// The test creates two DIFFERENT models with DIFFERENT model IDs, takes a snapshot
// of one, then changes shared state to the other, and verifies that the summary
// uses the snapshot (proven by checking the summary message's Model field).
//
// REVERT CHECK PROCEDURE:
//  1. In runSummarizeSilent (agent.go:~3320), add these lines right after the signature:
//     // BUG: Re-read shared state instead of using snapshot (P1-3 revert check)
//     largeModel = a.largeModel.Get()
//     systemPromptPrefix = a.systemPromptPrefix.Get()
//  2. Run: go test ./internal/agent -run TestP1_3_SummaryUsesImmutableSnapshot -v
//  3. The test will FAIL because the summary will show "model-changed" instead of "model-a"
//  4. Restore the fix (remove the BUG lines) and the test will PASS again.
func TestP1_3_SummaryUsesImmutableSnapshot(t *testing.T) {
	t.Parallel()

	// Create TWO separate mock servers with DIFFERENT model IDs in their responses.
	var callsA atomic.Int64
	var callsB atomic.Int64

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Stream with model-a in the response.
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
	t.Cleanup(srvA.Close)

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Stream with model-changed in the response.
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-changed","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-changed","choices":[{"index":0,"delta":{"content":"summary"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-changed","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`,
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
	t.Cleanup(srvB.Close)

	// Build two DIFFERENT models from the two servers.
	providerA, err := openaicompat.New(
		openaicompat.WithBaseURL(srvA.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lmA, err := providerA.LanguageModel(context.Background(), "model-a")
	require.NoError(t, err)

	providerB, err := openaicompat.New(
		openaicompat.WithBaseURL(srvB.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lmB, err := providerB.LanguageModel(context.Background(), "model-changed")
	require.NoError(t, err)

	// Build two DIFFERENT Model structs.
	modelA := Model{
		Model:      lmA,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg: config.SelectedModel{
			Provider: "openaicompat",
			Model:    "model-a",
		},
	}
	modelB := Model{
		Model:      lmB,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg: config.SelectedModel{
			Provider: "openaicompat",
			Model:    "model-changed",
		},
	}

	// Build the test environment with model-a.
	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           modelA,
		SmallModel:           modelA,
		SystemPrompt:         "you are a summarizer",
		SystemPromptPrefix:   "prefix-a",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sa := a.(*sessionAgent)

	// Create a session with enough messages to trigger summarization.
	sess, err := env.sessions.Create(context.Background(), "p1-3 snapshot test")
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

	ctx := context.Background()

	// Get the snapshot ONCE before calling runSummarizeSilent.
	// This simulates what runTurn does: resolve turnConfig once and pass it down.
	snapshotModel := sa.largeModel.Get()
	snapshotPrefix := sa.systemPromptPrefix.Get()

	// Verify that the snapshot is model-a.
	require.Equal(t, "model-a", snapshotModel.ModelCfg.Model, "snapshot should be model-a initially")
	require.Equal(t, "prefix-a", snapshotPrefix, "snapshot should be prefix-a initially")

	// Now change the shared state to model-b (DIFFERENT model ID).
	// If runSummarizeSilent re-reads shared state (bug), it will see model-changed.
	sa.SetModels(modelB, modelB)
	sa.SetSystemPromptPrefix("prefix-changed")

	// Verify the shared state was changed.
	changedModel := sa.largeModel.Get()
	changedPrefix := sa.systemPromptPrefix.Get()
	require.Equal(t, "model-changed", changedModel.ModelCfg.Model, "shared state should now be model-changed")
	require.Equal(t, "prefix-changed", changedPrefix, "shared state should now be prefix-changed")

	// Run the summary with the snapshot (not the changed shared state).
	err = sa.runSummarizeSilent(ctx, sess.ID, fantasy.ProviderOptions{}, snapshotModel, snapshotPrefix)
	require.NoError(t, err)

	// Verify that the provider-a was called (not provider-b).
	require.Equal(t, int64(1), callsA.Load(), "summary stream should have called provider-a")
	require.Equal(t, int64(0), callsB.Load(), "summary stream should NOT have called provider-b")

	// Fetch all messages and check the summary message's Model field.
	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	var summaryMsg *message.Message
	for i := range msgs {
		if msgs[i].IsSummaryMessage {
			summaryMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, summaryMsg, "summary message should exist")

	// THIS IS THE KEY ASSERTION: the summary message should have model-a
	// (from the snapshot), NOT model-changed (from the changed shared state).
	// Without the P1-3 fix, runSummarizeSilent would re-read shared state and
	// the summary would have model-changed.
	require.Equal(t, "model-a", summaryMsg.Model,
		"summary should use snapshot model-a, not changed shared state model-changed")
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
		ID:   "openaicompat",
		Type: openaicompat.Name,
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
	err = c.Summarize(t.Context(), sess.ID)
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

func (m *modelCallSpyAgent) SetTools(tools []fantasy.AgentTool) {}
func (m *modelCallSpyAgent) SetSystemPrompt(systemPrompt string) {
}

func (m *modelCallSpyAgent) Model() Model {
	m.modelCallCount.Add(1)
	return m.model
}
func (m *modelCallSpyAgent) Cancel(sessionID string)                     {}
func (m *modelCallSpyAgent) CancelAll()                                  {}
func (m *modelCallSpyAgent) IsSessionBusy(sessionID string) bool         { return false }
func (m *modelCallSpyAgent) IsBusy() bool                                { return false }
func (m *modelCallSpyAgent) QueuedPrompts(sessionID string) int          { return 0 }
func (m *modelCallSpyAgent) QueuedPromptsList(sessionID string) []string { return nil }
func (m *modelCallSpyAgent) ClearQueue(sessionID string)                 {}
func (m *modelCallSpyAgent) QueueMessage(call SessionAgentCall)          {}
func (m *modelCallSpyAgent) InterruptAndReplace(_ string, call SessionAgentCall) bool {
	return true
}

func (m *modelCallSpyAgent) InjectMessage(_ context.Context, call SessionAgentCall) (message.Message, error) {
	return message.Message{SessionID: call.SessionID}, nil
}

func (m *modelCallSpyAgent) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
	// Call the real runSummarizeSilent to actually create a summary.
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           m.model,
		SmallModel:           m.model,
		SystemPrompt:         "you are a summarizer",
		SystemPromptPrefix:   "",
		IsYolo:               true,
		Sessions:             m.sessions,
		Messages:             m.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)
	largeModel := sessionAgent.largeModel.Get()
	systemPromptPrefix := sessionAgent.systemPromptPrefix.Get()
	return sessionAgent.runSummarizeSilent(ctx, sessionID, opts, largeModel, systemPromptPrefix)
}
func (m *modelCallSpyAgent) SummarizeQueued(string) bool { return false }
func (m *modelCallSpyAgent) TakeSummarizeQueue(string) (fantasy.ProviderOptions, bool) {
	return fantasy.ProviderOptions{}, false
}
func (m *modelCallSpyAgent) CancelQueuedSummarize(string)          {}
func (m *modelCallSpyAgent) SetSystemPromptPrefix(string)          {}
func (m *modelCallSpyAgent) SystemPrompt() string                  { return "" }
func (m *modelCallSpyAgent) SetTimeoutOptions(bool, time.Duration) {}
