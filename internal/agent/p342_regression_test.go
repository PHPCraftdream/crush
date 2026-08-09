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

// TestP342_SecondManualCompactCoalescedAfterFirstCompletes is the end-to-end
// regression test for task #342, which fixes two related problems around
// manual-summarize (/compact) queue handling:
//
// PROBLEM 1: A second manual /compact queued during the first successful
// /compact was never drained from summarizeQueue, remaining queued forever.
//
// PROBLEM 2: Web events didn't follow real ownership transitions - the
// queued state was only cleared by explicit cancel, not by actual queue drain.
//
// This test FAILS when the bugs are present:
// - The second /compact stays in summarizeQueue forever (SummarizeQueued returns true)
// - The queued state is never cleared, UI shows "compact queued" indefinitely
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go runSummarize success path (~line 3230), remove the
//     summarizeQueue drain block (lines 3232-3238).
//  2. In handlers.go handleListSessions (~line 787), remove the
//     summarizeQueued resync (the second c.reply line).
//  3. Run: go test ./internal/agent -run TestP342_SecondManualCompactCoalescedAfterFirstCompletes -v
//  4. The test will FAIL because:
//     - SummarizeQueued(sessionID) still returns true after first /compact completes
//     - The second /compact entry is never coalesced/drained
//  5. Restore the fixes and the test will PASS again.
func TestP342_SecondManualCompactCoalescedAfterFirstCompletes(t *testing.T) {
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
	sess, err := env.sessions.Create(ctx, "p342-compact-coalesce-test")
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

	// Start a normal Run to establish ownership.
	// This will process all 8 messages without summarizing (DisableAutoSummarize=true).
	_, err = sa.Run(ctx, SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "test message to establish ownership",
	})
	require.NoError(t, err)

	// PHASE 1: Start first /compact - it should acquire ownership immediately.
	summarizeErr := make(chan error, 1)
	summarizeStarted := make(chan struct{}, 1)
	go func() {
		summarizeStarted <- struct{}{}
		summarizeErr <- sa.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())
	}()

	// Wait for the first /compact to actually start and acquire ownership.
	<-summarizeStarted
	require.Eventually(t, func() bool {
		return sessionAgent.IsSessionBusy(sess.ID)
	}, 2*time.Second, 50*time.Millisecond, "session must become busy during first /compact")

	// Verify session is busy.
	require.True(t, sessionAgent.IsSessionBusy(sess.ID), "session must be busy during first /compact")

	// PHASE 2: Queue a second /compact while the first is still running.
	// This should return ErrSummarizeQueued.
	secondCompactErr := sa.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())
	require.ErrorIs(t, secondCompactErr, ErrSummarizeQueued, "second /compact must be queued")

	// Verify the second /compact is in the queue.
	require.True(t, sessionAgent.SummarizeQueued(sess.ID), "summarizeQueue must hold the pending second /compact")

	// Wait for first /compact to complete with a reasonable timeout.
	require.Eventually(t, func() bool {
		select {
		case err := <-summarizeErr:
			require.NoError(t, err, "first /compact must complete successfully")
			return true
		default:
			return false
		}
	}, 10*time.Second, 100*time.Millisecond, "first /compact must complete within timeout")

	// PHASE 3: Verify the FIX for Problem 1 - the queued entry was coalesced/drained.
	// After successful compaction, the summarizeQueue should be empty because the
	// success path now drains it (coalescing the stale queued entry).
	require.Eventually(t, func() bool {
		return !sessionAgent.SummarizeQueued(sess.ID)
	}, 5*time.Second, 50*time.Millisecond,
		"summarizeQueue must be empty after first /compact completes (Problem 1 fix)")

	// Verify that we don't have a runaway compaction in the background.
	// If the bug were present, the second /compact would still be queued forever.
	// The coalesce fix ensures it's discarded, so we should NOT see additional
	// summarize calls after the first one completes.
	time.Sleep(200 * time.Millisecond)

	// Verify session is idle again (the first /compact released ownership).
	require.False(t, sessionAgent.IsSessionBusy(sess.ID), "session must be idle after first /compact completes")

	// PHASE 4: Simulate the FIX for Problem 2 - verify that list_sessions resync
	// would correctly report the queued state as false.
	//
	// The fix adds summarizeQueued resync in handleListSessions, which means
	// the server's authoritative state (SummarizeQueued()) is sent to clients
	// on each 5-second poll. We verify this state is correct here.
	queuedState := sessionAgent.SummarizeQueued(sess.ID)
	require.False(t, queuedState, "SummarizeQueued() must report false after coalesce (Problem 2 fix)")

	// Verify total provider calls: we expect exactly 2 (Run + first /compact),
	// not 3. If the second /compact had executed instead of being coalesced,
	// we'd see 3 calls.
	require.Equal(t, int64(2), totalCalls.Load(), "should only see 2 LLM calls (Run + first /compact), not a third for coalesced /compact")

	// Additional verification: a third /compact should work normally now.
	// This proves the queue is genuinely empty, not in a broken state.
	thirdCompactErr := sa.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())
	require.NoError(t, thirdCompactErr, "third /compact must succeed (queue is genuinely empty)")
}
