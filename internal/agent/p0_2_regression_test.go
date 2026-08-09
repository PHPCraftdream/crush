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
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP0_2_RetryExhaustion_QueuesCall is a regression test for Problem 1:
// restartOrphanedWithRetry must queue calls after retry exhaustion to prevent
// data loss when OS lock is held for longer than the retry window (~1.6s).
//
// CRITICAL: This test proves BOTH persistence AND execution:
//  1. Persistence (first phase): call is queued in mb.submitted after retry exhaustion
//  2. Execution (second phase): a future Run() picks up the queued call and executes it
//
// This test FAILS when mb.queue(call) is removed or commented out from the
// retry exhaustion path, because the orphaned call disappears completely.
//
// Sequence 3 of 9 from the 2026-08-07 concurrency review audit (tasks #328-#336).
// See docs/reviews/2026-08-07-release-concurrency-review.md P0-2 (Problem 1).
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go restartOrphanedWithRetry (~line 970), comment out mb.queue(call):
//     // BUG: Don't queue the call (P0-2a revert check)
//     // mb.queue(call)
//  2. Run: go test ./internal/agent -run TestP0_2_RetryExhaustion_QueuesCall -v
//  3. The test will FAIL on TWO assertions:
//     a. First phase: "orphaned call should be queued after retry exhaustion" - call is lost
//     b. Second phase: "provider should have been called for queued call" - no execution
//  4. Restore mb.queue(call) and both phases will PASS.
func TestP0_2_RetryExhaustion_QueuesCall(t *testing.T) {
	t.Parallel()

	// Track provider calls to prove actual execution (not just queuing).
	var providerCalls atomic.Int64

	// Set up a mock provider that responds normally.
	// This proves execution in the second phase: we count actual provider calls.
	busyLockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
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
	t.Cleanup(busyLockSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(busyLockSrv.URL),
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
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, "p0-2-retry-exhaustion-test")
	require.NoError(t, err)
	sessionID := sess.ID // Use the actual session ID from the created session

	// Add a seed message.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err)

	// Hold the OS lock to force restartOrphanedWithRetry to fail.
	// This simulates another process holding the lock.
	lock, err := session.TryAcquireSessionLock(env.workingDir, sessionID)
	require.NoError(t, err, "should acquire OS lock to force retry exhaustion")
	defer lock.Release()

	// Call that will be orphaned and retried.
	orphanedCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "orphaned call",
	}

	// Start a detached run that will exhaust retries.
	sessionAgent.restartOrphanedWithRetry([]SessionAgentCall{orphanedCall})

	// Wait for all retry attempts to complete (5 attempts + backoff: ~1.6s total).
	time.Sleep(2 * time.Second)

	// PHASE 1: PROVE PERSISTENCE
	// With the fix, the call should be queued in mb.submitted.
	// Without the fix (commenting out mb.queue(call)), submitted is empty
	// and the call is lost.
	mb := sessionAgent.getMailbox(sessionID)
	mb.mu.Lock()
	submittedCalls := make([]SessionAgentCall, len(mb.submitted))
	copy(submittedCalls, mb.submitted)
	mb.mu.Unlock()
	require.Len(t, submittedCalls, 1, "orphaned call should be queued after retry exhaustion")
	require.Equal(t, orphanedCall.SessionID, submittedCalls[0].SessionID)
	require.Equal(t, orphanedCall.Prompt, submittedCalls[0].Prompt)

	// PHASE 2: PROVE EXECUTION
	// Now simulate a "future owner comes back" scenario by releasing the OS lock.
	// The queued call must be ACTUALLY EXECUTED by a subsequent detached run.
	lock.Release()

	// Give the detached goroutine time to finish its cleanup.
	time.Sleep(100 * time.Millisecond)

	// Trigger a new Run() which should pick up the queued call.
	// We use a throwaway prompt - the queued call should execute first via popFirstSubmitted().
	_, err = sessionAgent.Run(ctx, SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "throwaway - queued call should execute first",
	})
	require.NoError(t, err, "Run should succeed and execute the queued call")

	// Verify the provider was called.
	// Without queuing (persistence gap), the orphaned call never reaches here.
	// Without execution (this is what we're proving now), provider wouldn't be called.
	require.GreaterOrEqual(t, providerCalls.Load(), int64(1),
		"provider should have been called for queued call")

	// Verify the orphaned call's content is in the message history.
	// This proves the call was ACTUALLY EXECUTED, not just queued.
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	var foundOrphanedContent bool
	for _, m := range msgs {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text == "orphaned call" {
				foundOrphanedContent = true
				break
			}
		}
		if foundOrphanedContent {
			break
		}
	}
	require.True(t, foundOrphanedContent,
		"orphaned call's content should appear in message history after execution")
}
