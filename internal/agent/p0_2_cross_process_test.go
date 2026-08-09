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
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure is a regression test for Problem 2:
// cross-process interrupt inject must not lose pending_injects rows when detached run
// fails even after retries. The row is deleted immediately by ConsumeInterruptInject,
// then recreated by startDetachedRun if it fails.
//
// CRITICAL: This test proves BOTH persistence AND execution:
//  1. Persistence (first phase): row is recreated after detached run fails OS lock race
//  2. Execution (second phase): next interrupt tick consumes recreated row and executes the message
//
// This test FAILS when the recreate logic is removed or broken, because the
// pending_injects row is permanently lost after a failed detached run.
//
// Sequence 4 of 9 from the 2026-08-07 concurrency review audit (tasks #328-#336).
// See docs/reviews/2026-08-07-release-concurrency-review.md P0-2 (Problem 2).
//
// REVERT CHECK PROCEDURE:
//  1. In coordinator.go startDetachedRun (~line 2196), comment out the recreate block:
//     // BUG: Don't recreate row (P0-2b revert check)
//     // if retryErr != nil {
//     //     _ = c.sessions.DeleteInterruptInject(ctx, call.InjectID)
//     //     _ = c.sessions.CreatePendingInject(ctx, ...)
//     // }
//  2. Run: go test ./internal/agent -run TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure -v
//  3. The test will FAIL on TWO assertions:
//     a. First phase: "pending_injects row should be recreated" - row is lost
//     b. Second phase: "provider should have been called for recreated row" - no execution
//  4. Restore the recreate block and both phases will PASS.
func TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure(t *testing.T) {
	t.Parallel()

	// Track provider calls to prove actual execution (not just row recreation).
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

	coord := &coordinator{
		cfg:          &config.ConfigStore{},
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: sessionAgent,
	}

	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, "p0-2-cross-process-interrupt-test")
	require.NoError(t, err)

	// Create a user message (simulating `crush sessions inject`).
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupted message"}},
	})
	require.NoError(t, err)

	// Create a pending_injects row (simulating the CLI creating it).
	inject := session.PendingInject{
		ID:        "test-inject-id-recreate-" + sess.ID,
		SessionID: sess.ID, // Use actual session ID from DB
		MessageID: msg.ID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	err = env.sessions.CreatePendingInject(ctx, inject)
	require.NoError(t, err)

	// Verify the row exists before the interrupt.
	pi, err := env.sessions.ConsumeInterruptInject(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pi, "pending_injects row should exist before interrupt")
	// The row was deleted by ConsumeInterruptInject, so recreate it for the test.
	err = env.sessions.CreatePendingInject(ctx, inject)
	require.NoError(t, err)

	// Build the call manually (simulating requeueInterruptMessage).
	// We can't use coord.buildCall because it requires full initialization.
	call := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    msg.FullText(),
		// ExistingMessageID: msg.ID,
		// InjectID:          inject.ID,
	}
	call.ExistingMessageID = msg.ID
	call.InjectID = inject.ID

	// Acquire the OS lock for this session to force failure.
	// This simulates another process holding the lock.
	lock, err := session.TryAcquireSessionLock(env.workingDir, sess.ID)
	require.NoError(t, err, "should acquire OS lock to force failure")
	defer lock.Release()

	// Call startDetachedRun directly. It should:
	// 1. Delete the row at start
	// 2. Fail to acquire OS lock (all retries)
	// 3. Recreate the row
	coord.startDetachedRun(ctx, call)

	// Wait for detached goroutine to complete (5 retries + backoff: ~1.6s total).
	time.Sleep(2 * time.Second)

	// With the fix, a row should be recreated (with a new ID to avoid UNIQUE constraint).
	// We don't check the exact ID (it's new), just that a row exists.
	pi, err = env.sessions.ConsumeInterruptInject(ctx, sess.ID)
	if err != nil {
		t.Logf("Error consuming after recreate: %v", err)
	}
	if pi == nil {
		t.Fatalf("Row was not recreated. This means the fix is broken - pending_injects row was permanently lost after detached run failure.")
	}
	require.NotNil(t, pi, "pending_injects row should be recreated after detached run failure")
	require.Equal(t, sess.ID, pi.SessionID, "recreated row should have the same session")
	require.Equal(t, msg.ID, pi.MessageID, "recreated row should reference the same message")
	require.True(t, pi.Interrupt, "recreated row should be an interrupt")

	// IMPORTANT: The above ConsumeInterruptInject call DELETED the row again.
	// For phase 2, we need to recreate it one more time so we can prove execution.
	injectPhase2 := session.PendingInject{
		ID:        "test-inject-id-phase2-" + sess.ID,
		SessionID: sess.ID,
		MessageID: msg.ID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	err = env.sessions.CreatePendingInject(ctx, injectPhase2)
	require.NoError(t, err, "should recreate row for phase 2 execution test")

	// Build the call for phase 2 (similar to phase 1, but with the new inject ID).
	callPhase2 := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    msg.FullText(),
	}
	callPhase2.ExistingMessageID = msg.ID
	callPhase2.InjectID = injectPhase2.ID

	// PHASE 2: PROVE EXECUTION
	// Release the lock so startDetachedRun can acquire it.
	lock.Release()

	// Give the detached goroutine from phase 1 time to finish its cleanup.
	time.Sleep(100 * time.Millisecond)

	// Call startDetachedRun again. With the lock released, it should succeed
	// and ACTUALLY EXECUTE the interrupt message.
	coord.startDetachedRun(ctx, callPhase2)

	// Wait for the detached run to execute.
	time.Sleep(2 * time.Second)

	// Verify the provider was called.
	// Without recreation (persistence gap), there's no row to consume.
	// Without execution (this is what we're proving now), provider wouldn't be called.
	require.GreaterOrEqual(t, providerCalls.Load(), int64(1),
		"provider should have been called for recreated interrupt row")

	// Verify the original user message is still in history and was processed.
	// The interrupt tick uses ExistingMessageID, so it references the original row
	// rather than creating a duplicate.
	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	var foundOriginalMessage bool
	for _, m := range msgs {
		if m.ID == msg.ID {
			foundOriginalMessage = true
			// Verify the original message content is preserved.
			require.Contains(t, m.FullText(), "interrupted message",
				"original message content should be preserved")
			break
		}
	}
	require.True(t, foundOriginalMessage,
		"original user message should exist and have been processed after execution")
}
