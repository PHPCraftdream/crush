package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot is the
// end-to-end regression test for task #341, P1-1: manual/queued /compact
// (summarize) must use a single immutable snapshot of model/provider-options/
// prompt-prefix resolved from the TARGET session, not from process-global
// shared state.
//
// This test FAILS when the bug is present (coordinator.Summarize reads shared
// state multiple times, runSummarize reads it again, or summarizeQueue stores
// only ProviderOptions).
//
// REVERT CHECK PROCEDURE:
//  1. In coordinator.go, replace Summarize with the buggy version:
//     agentModel := c.currentAgent.Model()
//     providerCfg, ok := c.cfg.Config().Providers.Get(agentModel.ModelCfg.Provider)
//     ...
//     summarize := func() error {
//     return c.currentAgent.Summarize(ctx, sessionID, getProviderOptions(agentModel, providerCfg))
//     }
//  2. In agent.go runSummarize (~line 3166), restore the buggy line:
//     cfg := a.resolveTurnConfig(SessionAgentCall{})
//     err := a.runSummarizeBody(genCtx, sessionID, opts, cfg.largeModel, cfg.promptPrefix)
//  3. In agent.go, change summarizeQueue back to:
//     summarizeQueue *csync.Map[string, fantasy.ProviderOptions]
//  4. Update agent.Summarize to queue fantasy.ProviderOptions{} instead of snapshot
//  5. Run: go test ./internal/agent -run TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot -v
//  6. The test will FAIL with "summary used wrong model" or mismatched call counts
//  7. Restore the fix and the test will PASS again.
func TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot(t *testing.T) {
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

		// Stream with model-b in the response.
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-b","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-b","choices":[{"index":0,"delta":{"content":"summary"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"model-b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`,
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

	// Build TWO DIFFERENT models from the two servers.
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
	lmB, err := providerB.LanguageModel(context.Background(), "model-b")
	require.NoError(t, err)

	// Build TWO DIFFERENT Model structs.
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
			Model:    "model-b",
		},
	}

	// Build the test environment with model-a as the initial shared state.
	env := testEnv(t)
	providerCfgA := config.ProviderConfig{
		ID:                 "openaicompat",
		Type:               openaicompat.Name,
		BaseURL:            srvA.URL,
		APIKey:             "probe",
		SystemPromptPrefix: "prefix-a",
		Models: []catwalk.Model{
			{ID: "model-a", Name: "model-a", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	}
	providerCfgB := config.ProviderConfig{
		ID:                 "openaicompat",
		Type:               openaicompat.Name,
		BaseURL:            srvB.URL,
		APIKey:             "probe",
		SystemPromptPrefix: "prefix-b",
	}

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set("openaicompat", providerCfgA)
	// NewCoordinator's buildAgentModels needs a selected large/small model
	// to construct the coordinator at all — found via a CI-only failure
	// ("large model not selected") that never reproduced locally (some
	// unidentified environment leak on the dev machine apparently made this
	// already true). The actual value here only matters for this initial
	// construction: coord.currentAgent.SetModels(modelA, modelA) right below
	// immediately overwrites it with this test's own directly-built models.
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeLarge, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "model-a",
	})
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeSmall, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "model-a",
	})

	c, err := NewCoordinator(t.Context(), cfg, env.sessions, env.messages, env.permissions, env.history, *env.filetracker, nil)
	require.NoError(t, err)
	coord := c.(*coordinator)

	// Start with model-a in the shared agent state.
	coord.currentAgent.SetModels(modelA, modelA)
	coord.currentAgent.SetSystemPromptPrefix("prefix-a")

	// Create TWO sessions: sessionA and sessionB.
	sessionA, err := env.sessions.Create(t.Context(), "session-a")
	require.NoError(t, err)
	sessionB, err := env.sessions.Create(t.Context(), "session-b")
	require.NoError(t, err)

	// Create 8 messages in sessionA (4 user + 4 assistant) to trigger summarization.
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(t.Context(), sessionA.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Create 8 messages in sessionB to trigger summarization.
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(t.Context(), sessionB.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Deterministically land the concurrent shared-state mutation exactly
	// inside the window a pre-#341 regression would have re-read shared
	// state in — mailbox.testPreSnapshotConsumeSeam fires strictly after
	// coordinator.Summarize captured its snapshot (model-a) and strictly
	// before runSummarize consumes it. A wall-clock time.Sleep here would
	// NOT be reliable: this environment's in-memory beginCompact/OS-lock
	// path can complete well inside a few milliseconds, so a sleep-based
	// "concurrent" mutation can simply arrive too late to ever land in the
	// vulnerable window — confirmed via this test's own revert-check
	// passing 5/5 even with the bug reintroduced when synchronized by sleep.
	sa := coord.currentAgent.(*sessionAgent)
	mb := sa.getMailbox(sessionA.ID)
	seamHit := make(chan struct{})
	mb.testPreSnapshotConsumeSeam = func() {
		defer close(seamHit)
		// This simulates the exact scenario where the bug would manifest:
		// - coordinator.Summarize already built its snapshot from shared
		//   state (model-a, captured before this seam fires)
		// - the pre-#341 runSummarize would re-read shared state here and
		//   see model-b instead
		coord.currentAgent.SetModels(modelB, modelB)
		coord.currentAgent.SetSystemPromptPrefix("prefix-b")
		cfg.Config().Providers.Set("openaicompat", providerCfgB)
	}
	t.Cleanup(func() { mb.testPreSnapshotConsumeSeam = nil })

	// Pass nil to Summarize - it will resolve the snapshot from current shared state.
	err = coord.Summarize(t.Context(), sessionA.ID, nil)
	require.NoError(t, err, "summarize must succeed")

	select {
	case <-seamHit:
	default:
		t.Fatal("testPreSnapshotConsumeSeam never fired — the seam wiring itself is broken, this test proves nothing")
	}

	// KEY ASSERTION: only providerA should have been called (model-a).
	// If the bug is present, providerB might also be called or providerA might
	// be called with model-b's provider options (which would fail or produce wrong results).
	require.Equal(t, int64(1), callsA.Load(),
		"summary must call provider-a (model-a), got %d calls", callsA.Load())
	require.Equal(t, int64(0), callsB.Load(),
		"summary must NOT call provider-b (model-b), got %d calls", callsB.Load())

	// Fetch all messages and verify the summary message's Model field.
	msgs, err := env.messages.List(t.Context(), sessionA.ID)
	require.NoError(t, err)

	var summaryMsg *message.Message
	for i := range msgs {
		if msgs[i].IsSummaryMessage {
			summaryMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, summaryMsg, "summary message should exist")

	// The summary message must have model-a (from the target session).
	require.Equal(t, "model-a", summaryMsg.Model,
		"summary must use model-a from target session, not model-b from concurrent shared state change")
}
