package agent

import (
	"context"
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
	"github.com/stretchr/testify/require"
)

// TestP2_1_SummarizeQueueDrainedFromNonWebPath verifies that summarizeQueue
// is drained from abandonOwnershipWithHandoff when the session becomes idle,
// even when ownership transitions via non-web paths (not the web handler).
//
// This test FAILS when the fix is removed from abandonOwnershipWithHandoff:
// the summarize request stays queued forever and never executes.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go abandonOwnershipWithHandoff (~line 985), comment out the
//     entire summarizeQueue drain block (lines 985-994).
//  2. Run: go test ./internal/agent -run TestP2_1_SummarizeQueueDrainedFromNonWebPath -v
//  3. The test will FAIL because summarize is never executed (queue never drains).
//  4. Restore the fix and the test will PASS again.
func TestP2_1_SummarizeQueueDrainedFromNonWebPath(t *testing.T) {
	t.Parallel()

	// Create a fake SSE provider for both Run and summarize.
	var totalCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Stream a simple response.
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"response"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`,
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

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "test-model")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg: config.SelectedModel{
			Provider: "openaicompat",
			Model:    "test-model",
		},
	}

	// Create the test environment with the model.
	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are an assistant",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()

	// Create a session.
	sess, err := env.sessions.Create(ctx, "p2-1-non-web-summarize-test")
	require.NoError(t, err)

	// Create enough messages to trigger summarization (4 user + 4 assistant).
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Track when Run starts and completes.
	runStarted := make(chan struct{}, 1)
	runDone := make(chan struct{}, 1)

	// Start a long-running Run in the background (this establishes ownership).
	runCall := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "long-running turn",
	}
	go func() {
		// Signal that Run has started (before actually running).
		runStarted <- struct{}{}
		// Give the signal time to propagate.
		time.Sleep(1 * time.Millisecond)
		// Now actually run.
		defer close(runDone)
		_, _ = sessionAgent.Run(ctx, runCall)
	}()

	// Wait for Run to actually start and acquire ownership.
	require.Eventually(t, func() bool {
		return sessionAgent.IsSessionBusy(sess.ID)
	}, 1*time.Second, 10*time.Millisecond, "session should become busy after Run starts")

	// Verify the session is busy (owned by the Run).
	require.True(t, sessionAgent.IsSessionBusy(sess.ID), "session should be busy after Run starts")

	// Call Summarize directly (NON-WEB PATH) while the session is busy.
	// This should queue the request and return ErrSummarizeQueued.
	err = sessionAgent.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())
	require.Error(t, err, "Summarize should return an error when session is busy")
	require.ErrorIs(t, err, ErrSummarizeQueued, "error should be ErrSummarizeQueued")

	// Verify the summarize was queued (SummarizeQueued returns true).
	require.True(t, sessionAgent.SummarizeQueued(sess.ID), "summarize should be queued")

	// Wait for the Run to complete (this will trigger abandonOwnershipWithHandoff,
	// which should drain the summarizeQueue and execute the summarize).
	select {
	case <-runDone:
		// Good - Run completed.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not complete within timeout")
	}

	// Give the detached summarize goroutine time to complete.
	// The fix runs Summarize in a detached goroutine with context.Background(),
	// so it may take a moment to finish.
	require.Eventually(t, func() bool {
		return totalCalls.Load() >= 2 // Run + summarize
	}, 2*time.Second, 50*time.Millisecond, "summarize provider should have been called")

	// Verify the summarize was executed (SummarizeQueued returns false).
	require.Eventually(t, func() bool {
		return !sessionAgent.SummarizeQueued(sess.ID)
	}, 2*time.Second, 50*time.Millisecond, "summarize queue should be empty after execution")

	// Verify that the provider was called for both Run and summarize (total = 2).
	require.Eventually(t, func() bool {
		return totalCalls.Load() >= 2
	}, 2*time.Second, 50*time.Millisecond, "provider should have been called twice (Run + summarize)")

	// Verify the summary message was added to the history.
	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	var foundSummary bool
	for _, msg := range msgs {
		if msg.IsSummaryMessage {
			foundSummary = true
			break
		}
	}
	require.True(t, foundSummary, "summary message should exist in message history")

	// Verify the provider was called exactly twice (Run + summarize).
	require.Equal(t, int64(2), totalCalls.Load(), "provider should have been called twice (Run + summarize)")
}
