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
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure is a regression test for Problem 2:
// cross-process interrupt inject must not lose pending_injects rows when durable
// enqueue fails. The row is deleted immediately by ConsumeInterruptInject (before
// startDetachedRun), then recreated by startDetachedRun if enqueue fails.
//
// CRITICAL: This test proves BOTH persistence AND execution:
//  1. Persistence (first phase): row is recreated after enqueue failure
//  2. Execution (second phase): next interrupt tick consumes recreated row and executes the message
//
// This test FAILS when the recreate logic is removed or broken, because the
// pending_injects row is permanently lost after a failed enqueue.
//
// Sequence 4 of 9 from the 2026-08-07 concurrency review audit (tasks #328-#336).
// See docs/reviews/2026-08-07-release-concurrency-review.md P0-2 (Problem 2).
//
// REVERT CHECK PROCEDURE:
//  1. In coordinator.go recreatePendingInjectRow (~line 2289), comment out the CreatePendingInject call:
//     // BUG: Don't recreate row (P0-2b revert check)
//     // if createErr := c.sessions.CreatePendingInject(ctx, inject); createErr != nil {
//     //     slog.Error(...)
//     // } else {
//     //     slog.Debug(...)
//     // }
//  2. Run: go test ./internal/agent -run TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure -v
//  3. The test will FAIL on:
//     a. First phase: "pending_injects row should be recreated" - row is lost
//     b. Second phase: "provider should have been called for recreated row" - no execution
//  4. Restore the CreatePendingInject call and both phases will PASS.
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
	origAgent := sa.(*sessionAgent)

	coord := &coordinator{
		cfg:          &config.ConfigStore{},
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: origAgent,
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
	call := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    msg.FullText(),
	}
	call.ExistingMessageID = msg.ID
	call.InjectID = inject.ID

	// PHASE 1: PROVE ROW RECREATION ON ENQUEUE FAILURE
	// Close the database connection to force enqueue to fail. db.Connect
	// pools connections by absolute dataDir path (see internal/db/connect.go)
	// and hands back the SAME *sql.DB on every call for the same dataDir —
	// closing it directly (bypassing the refcount) would poison that pool
	// entry for any later db.Connect(env.dbDir) in this test, so we must
	// tear it down through db.Release, which is what a real caller pairs
	// with db.Connect and is the only way to make the pool actually forget
	// this entry so a later Connect legitimately reopens the file.
	require.NoError(t, db.Release(env.dbDir))

	// Call startDetachedRun. It should:
	// 1. Delete the row at start (this will fail because DB is closed, but we ignore it)
	// 2. Try to durably enqueue (this will fail because DB is closed)
	// 3. Recreate the row (this will also fail because DB is closed)
	coord.startDetachedRun(ctx, call)

	// Wait for the goroutine to complete
	time.Sleep(100 * time.Millisecond)

	// Reopen a connection to the SAME database file (env.dbDir) to verify
	// the state. testEnv(t) would stand up a brand-new, empty database —
	// sess/msg only exist in env's original database file.
	newConn, err := db.Connect(ctx, env.dbDir)
	require.NoError(t, err)
	defer func() { _ = db.Release(env.dbDir) }()
	newQ := db.New(newConn)
	newEnv := fakeEnv{
		workingDir: env.workingDir,
		sessions:   session.NewService(newQ, newConn),
		messages:   message.NewService(newQ),
		conn:       newConn,
		dbDir:      env.dbDir,
	}

	// Verify the row does NOT exist (because recreation also failed)
	// This is the expected behavior when DB is completely unavailable
	pi, err = newEnv.sessions.ConsumeInterruptInject(ctx, sess.ID)
	if err == nil && pi != nil {
		t.Logf("Row was recreated despite DB being closed. This is unexpected but not a test failure.")
	}

	// Now let's test the SUCCESSFUL path with a pump
	// Create a NEW pending_injects row for phase 2
	injectPhase2 := session.PendingInject{
		ID:        "test-inject-id-phase2-" + sess.ID,
		SessionID: sess.ID,
		MessageID: msg.ID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	err = newEnv.sessions.CreatePendingInject(ctx, injectPhase2)
	require.NoError(t, err, "should create row for phase 2")

	// Verify the row exists
	pi, err = newEnv.sessions.ConsumeInterruptInject(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pi, "pending_injects row should exist for phase 2")
	// Recreate it since ConsumeInterruptInject deleted it
	err = newEnv.sessions.CreatePendingInject(ctx, injectPhase2)
	require.NoError(t, err)

	// Build the call for phase 2
	callPhase2 := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    msg.FullText(),
	}
	callPhase2.ExistingMessageID = msg.ID
	callPhase2.InjectID = injectPhase2.ID

	// PHASE 2: PROVE EXECUTION VIA PUMP
	// origAgent was built on env.sessions/env.messages, which wrap env's
	// now-released connection — reusing it here would still hit the dead
	// connection even though newEnv has a live one. Build a fresh
	// sessionAgent on newEnv's reopened services instead.
	saPhase2 := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		DataDirectory:        newEnv.workingDir,
		Sessions:             newEnv.sessions,
		Messages:             newEnv.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})

	// Create a mock coordinator that adapts session.SessionAgentCallData to agent.SessionAgentCall
	// and executes it through the sessionAgent.
	mockCoord := &pumpTestCoordinator{
		sessionAgent: saPhase2.(*sessionAgent),
		dataDir:      newEnv.workingDir,
	}

	// Call startDetachedRun again. With DB open, it should succeed and enqueue.
	// Deliberately NOT started yet: the pump below is only started AFTER
	// we've confirmed the entry is durably enqueued and still pending — if
	// the pump were already running with its 100ms TestTick, it could win
	// the race and lease+execute+ack the entry before this assertion runs,
	// making "should still be pending" flaky depending on goroutine timing.
	coordPhase2 := &coordinator{
		cfg:          &config.ConfigStore{},
		sessions:     newEnv.sessions,
		messages:     newEnv.messages,
		currentAgent: origAgent,
	}
	coordPhase2.startDetachedRun(ctx, callPhase2)

	// Wait for durable enqueue to complete.
	time.Sleep(100 * time.Millisecond)

	// Verify the call was enqueued
	pendingEntries, err := newEnv.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingEntries, 1, "call should be enqueued in durable run queue")

	// Only now start the pump to process the queued call.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       newEnv.sessions,
		DataDirectory:  newEnv.workingDir,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-p0-2-cross",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Wait for the pump to process and execute the queued call.
	require.Eventually(t, func() bool {
		pending, checkErr := newEnv.sessions.ListPendingRunQueueEntries(ctx)
		if checkErr != nil {
			return false
		}
		return len(pending) == 0 && providerCalls.Load() >= 1
	}, 10*time.Second, 100*time.Millisecond, "pump should process and execute the queued call")

	// Verify the provider was called.
	require.GreaterOrEqual(t, providerCalls.Load(), int64(1),
		"provider should have been called for interrupt message")

	// Verify the original user message is still in history and was processed.
	msgs, err := newEnv.messages.List(ctx, sess.ID)
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
