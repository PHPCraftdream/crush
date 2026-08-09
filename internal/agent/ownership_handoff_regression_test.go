package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_SynchronousRun_Regression
// is a regression test for the bug where abandonOwnershipWithHandoff was called
// unconditionally on the success path of runSummarize.
//
// This test actually calls the public Summarize API (which calls runSummarize
// internally) and proves that on success, the follow-up call runs synchronously
// and errors propagate correctly.
//
// The test FAILS when abandonOwnershipWithHandoff is called unconditionally on
// the success path, because:
// 1. abandonOwnershipWithHandoff calls popAllSubmitted() which clears the queue
// 2. popFirstSubmitted() in the success path returns nothing (hasNext == false)
// 3. The synchronous a.Run(ctx, firstQueued) never executes
// 4. The error from the follow-up call is lost (runSummarize returns nil instead)
//
// See agent.go line 2981-2986 for the revert-check setup.
func TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_SynchronousRun_Regression(t *testing.T) {
	t.Parallel()

	// Set up a simple test server that responds to summarization requests.
	summarizeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"summarized"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			w.Write([]byte("data: " + c + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(summarizeSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(summarizeSrv.URL),
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
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: false,
	})
	sessionAgent := sa.(*sessionAgent)

	sessionID := "regression-sync-run-test"
	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, sessionID)
	require.NoError(t, err)

	// Add seed messages to allow summarization to complete.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed message for summarization"}},
	})
	require.NoError(t, err)

	// Queue a follow-up call that uses an invalid (non-existent) session ID.
	// This will cause the synchronous a.Run(ctx, firstQueued) to fail with
	// "failed to get session" - this error should propagate as Summarize's return value.
	failingQueuedCall := SessionAgentCall{
		SessionID: "nonexistent-session-id-should-fail",
		Prompt:    "this should fail",
	}

	// Queue the failing call BEFORE Summarize acquires ownership.
	// This ensures it will be in the submitted queue when Summarize reaches
	// the success path.
	sessionAgent.getMailbox(sess.ID).queue(failingQueuedCall)
	require.Equal(t, 1, sessionAgent.QueuedPrompts(sess.ID), "failing call must be queued")

	// Call Summarize. With the FIX (correct code):
	// - Summarize succeeds (runSummarizeBody returns nil)
	// - On success path: mb.abandonOwnership(epoch) releases ownership
	// - popFirstSubmitted() returns the queued failing call (hasNext == true)
	// - a.Run(ctx, firstQueued) executes synchronously with the invalid session ID
	// - That a.Run fails with "failed to get session"
	// - The error propagates as Summarize's return value
	//
	// With the BUG (unconditional abandonOwnershipWithHandoff on success path):
	// - abandonOwnershipWithHandoff is called BEFORE the if err != nil check
	// - abandonOwnershipWithHandoff calls popAllSubmitted(), clearing the queue
	// - popFirstSubmitted() returns nothing (hasNext == false)
	// - The synchronous a.Run never executes
	// - Summarize returns nil (FALSE NEGATIVE)
	summarizeErr := sessionAgent.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())

	// The test proves the fix is in place by requiring Summarize to return an error.
	// With the buggy code, this assertion would FAIL because Summarize returns nil.
	require.Error(t, summarizeErr, "Summarize should return error from synchronous follow-up Run")
	require.Contains(t, summarizeErr.Error(), "failed to get session", "error should be from the follow-up Run with invalid session ID")
}
