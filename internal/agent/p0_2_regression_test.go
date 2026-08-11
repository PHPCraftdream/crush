package agent

import (
	"context"
	"encoding/json"
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
//  1. Persistence (first phase): call is queued in durable run queue after retry exhaustion
//  2. Execution (second phase): pump executes the queued call and completes it
//
// This test FAILS when a.sessions.EnqueueRunQueueEntry is removed or commented out from the
// retry exhaustion path, because the orphaned call disappears completely.
//
// Sequence 3 of 9 from the 2026-08-07 concurrency review audit (tasks #328-#336).
// See docs/reviews/2026-08-07-release-concurrency-review.md P0-2 (Problem 1).
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go restartOrphanedWithRetry (~line 1057), comment out EnqueueRunQueueEntry:
//     // BUG: Don't enqueue to durable queue (P0-2a revert check)
//     // if enqueueErr := a.sessions.EnqueueRunQueueEntry(context.Background(), idempotencyKey, call.SessionID, callDataJSON); enqueueErr != nil {
//  2. Run: go test ./internal/agent -run TestP0_2_RetryExhaustion_QueuesCall -v
//  3. The test will FAIL on:
//     a. First phase: "orphaned call should be queued in durable run queue" - call is lost
//     b. Second phase: "provider should have been called for queued call" - no execution
//  4. Restore the EnqueueRunQueueEntry call and both phases will PASS.
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

	// Wait for durable enqueue to complete.
	time.Sleep(100 * time.Millisecond)

	// PHASE 1: PROVE PERSISTENCE
	// With the fix, the call should be queued in the durable run queue.
	// Without the fix (commenting out EnqueueRunQueueEntry), the queue is empty
	// and the call is lost.
	pendingEntries, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingEntries, 1, "orphaned call should be queued in durable run queue after retry exhaustion")
	require.Equal(t, sessionID, pendingEntries[0].SessionID)

	// Verify the queued call data contains the expected prompt.
	var callData session.SessionAgentCallData
	err = json.Unmarshal([]byte(pendingEntries[0].CallData), &callData)
	require.NoError(t, err)
	require.Equal(t, "orphaned call", callData.Prompt, "queued call should contain correct prompt")

	// PHASE 2: PROVE EXECUTION
	// Now simulate a "future owner comes back" scenario by:
	// 1. Starting a pump to process queued calls
	// 2. Releasing the OS lock so the pump can acquire it
	// 3. Waiting for the call to execute

	// Create a mock coordinator that adapts session.SessionAgentCallData to agent.SessionAgentCall
	// and executes it through the sessionAgent.
	mockCoord := &pumpTestCoordinator{
		sessionAgent: sessionAgent,
		dataDir:      env.workingDir,
	}

	// Start a pump to process queued calls.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       env.sessions,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-p0-2",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Release the OS lock so the pump can acquire it.
	lock.Release()

	// Give the pump time to process the queued call.
	// We use Eventually to wait for:
	// 1. The queue to be empty (call was processed)
	// 2. The provider to be called
	// 3. The message to appear in history
	require.Eventually(t, func() bool {
		pending, checkErr := env.sessions.ListPendingRunQueueEntries(ctx)
		if checkErr != nil {
			return false
		}
		return len(pending) == 0 && providerCalls.Load() >= 1
	}, 10*time.Second, 100*time.Millisecond, "pump should process the queued call and provider should be called")

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

// pumpTestCoordinator adapts session.SessionAgentCallData to agent.SessionAgentCall
// and executes it through the sessionAgent for testing.
type pumpTestCoordinator struct {
	sessionAgent *sessionAgent
	dataDir      string
}

func (p *pumpTestCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	// Convert SessionAgentCallData back to SessionAgentCall using the existing conversion function
	call := FromSessionAgentCallData(callData)

	// Execute the call through the sessionAgent
	result, err := p.sessionAgent.Run(ctx, call)
	if err != nil {
		return nil, err
	}

	var anyResult any
	if result != nil {
		anyResult = result
	}
	return &anyResult, nil
}
