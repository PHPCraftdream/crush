package agent_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP0_338_FinalizerReachableDespiteHungCleanup empirically proves that the
// ownership finalizer (abandonOwnershipWithHandoff) in sessionAgent.Run is
// ALWAYS reachable, even if SessionLock.Release() hangs on diagnostic cleanup.
//
// This test proves BOTH reachability AND correct execution:
//  1. Reachability: Run() returns quickly despite hung cleanup, proving Release()
//     defer doesn't block the finalizer from being reached.
//  2. Execution: orphaned call (queued before Run) is actually executed by the
//     finalizer's detached run, proven by provider call count and message history.
//
// The test:
//  1. Injects a blocking version of clearHolderMetadataFn via the
//     package-global seam in internal/session/lock.go.
//  2. Starts a Run() that acquires the OS lock.
//  3. The first call to the provider returns an error via a custom context,
//     causing Run() to error out quickly and trigger the finalizer.
//  4. Verifies that Run() returns quickly (within 200ms) even though cleanup
//     is blocked — this proves Release() defer doesn't block.
//  5. Verifies that the finalizer abandonOwnershipWithHandoff actually ran
//     and correctly handed off the orphaned call via two proofs:
//     a. Provider was called at least twice (once for firstCall, once for orphanedCall)
//     b. orphanedCall.Prompt appears in the session's message history
//
// Since task #340 ROUND 3, the finalizer's detached run durably enqueues the
// orphaned call instead of executing it inline, so this test also starts a
// session.RunQueuePump (fast TestTick) to prove the queue is actually drained,
// not just written to.
//
// This is the SAME execution proof pattern as p0_2_regression_test.go
// (TestP0_2_RetryExhaustion_QueuesCall), using httptest.Server with SSE responses
// and atomic counters inside the HTTP handler.
//
// NOTE: This test is NOT parallel because it mutates the package-global
// clearHolderMetadataFn seam in internal/session/lock.go, which would create
// a data race with other parallel tests doing the same.
//
// REVERT CHECK PROCEDURE:
//  1. In lock.go Release(), change "go clearHolderMetadataFn(path)" back to
//     "clearHolderMetadataFn(l.Path)" (remove the "go " keyword).
//  2. Run: go test ./internal/agent -run TestP0_338_FinalizerReachableDespiteHungCleanup -v
//  3. The test will FAIL because Run() hangs forever on Release().
//  4. Restore the fix (add "go " back) and the test will PASS.
func TestP0_338_FinalizerReachableDespiteHungCleanup(t *testing.T) {
	tmpDir := t.TempDir()

	// Track provider calls to prove actual execution (not just queuing).
	var providerCalls atomic.Int64

	// Set up a mock provider that:
	// 1. Returns error on first call to trigger finalizer quickly
	// 2. Returns normal SSE responses on subsequent calls
	providerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNum := providerCalls.Add(1)

		if callNum == 1 {
			// First call: return error immediately to trigger finalizer
			t.Logf("Provider call %d: returning error to trigger finalizer", callNum)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"message":"synthetic provider failure","type":"invalid_request_error"}}`)
			return
		}

		t.Logf("Provider call %d: returning normal SSE response", callNum)
		// Subsequent calls: return normal SSE response
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"response"},"finish_reason":null}]}`,
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
	t.Cleanup(providerSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(providerSrv.URL),
		openaicompat.WithAPIKey("test-key"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := agent.Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
	}

	// Create DB and services.
	// Don't close DB immediately — we need it for detached runs.
	conn, err := db.Connect(t.Context(), tmpDir)
	require.NoError(t, err)
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	// Create the session.
	sess, err := sessions.Create(t.Context(), "test session for p0-338")
	require.NoError(t, err)
	sessionID := sess.ID

	// Add a message so the session isn't empty.
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test message"}},
	})
	require.NoError(t, err)

	// Create a sessionAgent with dataDir so it acquires the OS lock.
	sa := agent.NewSessionAgent(agent.SessionAgentOptions{
		DataDirectory: tmpDir,
		LargeModel:    model,
		SmallModel:    model,
		Sessions:      sessions,
		Messages:      messages,
		IsYolo:        true,
		SystemPrompt:  "You are a test assistant.",
	})

	// Queue an orphaned call BEFORE starting Run — it will become orphaned when Run fails.
	orphanedCall := agent.SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "orphaned call",
	}
	sa.QueueMessage(orphanedCall)

	// Inject the hung cleanup BEFORE starting Run, so it's active for the Release() defer.
	// We use the exported SetClearHolderMetadataFn to inject the hung cleanup.
	cleanupStarted := atomic.Bool{}
	cleanupUnblock := make(chan struct{})
	clearHolderMetadataOriginal := session.GetClearHolderMetadataFn()

	restoreFn := session.SetClearHolderMetadataFn(func(path string) {
		t.Logf("cleanup goroutine started for path: %s", path)
		cleanupStarted.Store(true)
		// Block for 2 seconds to prove the point, then unblock to avoid process cleanup issues.
		select {
		case <-time.After(2 * time.Second):
			t.Logf("cleanup goroutine unblocking after timeout")
			// Timeout - unblock and proceed with cleanup
		case <-cleanupUnblock:
			t.Logf("cleanup goroutine unblocking via explicit unblock")
			// Explicit unblock (used in cleanup phase)
		}
		clearHolderMetadataOriginal(path)
		t.Logf("cleanup goroutine completed")
	})
	defer func() {
		// Unblock cleanup before test completes to avoid leaving stuck goroutines
		close(cleanupUnblock)
		restoreFn()
	}()

	// Start the first Run that will acquire the lock and then fail.
	firstCall := agent.SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "first call",
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := sa.Run(t.Context(), firstCall)
		runErrCh <- err
	}()

	// Wait for Run() to return — CRITICAL: it should return quickly despite hung cleanup.
	// If the #337 fix is broken (Release() not running cleanup in background), this will hang forever.
	runStart := time.Now()
	select {
	case runErr := <-runErrCh:
		runDuration := time.Since(runStart)
		currentCalls := providerCalls.Load()
		t.Logf("Run() returned in %v with error: %v, provider calls: %d", runDuration, runErr, currentCalls)
		require.Less(t, runDuration, 200*time.Millisecond,
			"Run() should return within 200ms even with hung cleanup, got %v", runDuration)
		require.Error(t, runErr, "Run should fail")
		// The error message indicates Run() failed (expected since our provider returns HTTP 500).
		// What matters is that Run() returned quickly, proving Release() defer didn't block.
	case <-time.After(5 * time.Second):
		currentCalls := providerCalls.Load()
		t.Logf("Provider call count: %d", currentCalls)
		t.Logf("Cleanup started: %v", cleanupStarted.Load())
		require.Fail(t, "Run() did not return within 5 seconds - "+
			"this proves Release() is NOT running cleanup in background (fix #337 broken)")
	}

	// Wait for the cleanup goroutine to start (proves Release() reached it).
	// We wait AFTER Run() returns because cleanup goroutine is spawned in Release() defer,
	// which runs after Run() returns.
	deadline := time.After(2 * time.Second)
	for !cleanupStarted.Load() {
		select {
		case <-deadline:
			// If cleanup never started, Release() was never called.
			// This could happen if Run() failed before acquiring the OS lock.
			t.Logf("Provider call count: %d", providerCalls.Load())
			require.Fail(t, "cleanup goroutine did not start within 2 seconds - "+
				"Release() was never called (Run() may have failed before OS lock acquisition)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// CRITICAL VERIFICATION 1: The cleanup goroutine started and is blocked.
	// This proves Release() was called and the background cleanup goroutine was spawned.
	require.True(t, cleanupStarted.Load(),
		"Cleanup goroutine should have started (this proves Release() was called)")

	// CRITICAL VERIFICATION 2: The OS lock should be available even though cleanup is blocked.
	// This proves Release() returned after unlock/close, not after cleanup.
	//
	// We use Eventually with a short timeout because there might be a tiny race where
	// the background cleanup goroutine is reopening the file (for metadata clear) when
	// we try to acquire. The OS lock was already released by unlockFile() in Release(),
	// so this retry is just waiting for the file descriptor to be fully closed.
	var lk2 *session.SessionLock
	require.Eventually(t, func() bool {
		var err error
		lk2, err = session.TryAcquireSessionLock(tmpDir, sessionID)
		return err == nil && lk2 != nil
	}, 2*time.Second, 10*time.Millisecond,
		"OS lock should be acquirable even though cleanup is blocked")
	require.NotNil(t, lk2)
	if lk2 != nil {
		_ = lk2.Release()
	}

	// CRITICAL VERIFICATION 3: The finalizer's detached run actually executed the orphaned call.
	// We verify this by waiting for the provider call count to reach at least 2:
	//   - Call 1: firstCall (returns error, triggers finalizer)
	//   - Call 2+: orphaned call(s) executed by detached run
	//
	// Since task #340 ROUND 3, restartOrphanedWithRetry no longer executes the
	// call itself — it durably enqueues it and relies on a session.RunQueuePump
	// to pick it up. Without a pump running, the enqueued call would sit
	// pending forever, so we start one here (TestTick keeps its poll interval
	// short instead of the production 3s) before waiting for execution. This
	// is still NOT an "external poke": the finalizer autonomously durably
	// enqueued the call the moment Run() failed; the pump is just the
	// mandatory background executor for that queue, equivalent to what
	// App.New() wires in production.
	pumpCoord := &p0338PumpCoordinator{sessionAgent: sa}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       sessions,
		DataDirectory:  tmpDir,
		Coordinator:    pumpCoord,
		PumpInstanceID: "test-pump-p0-338",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	require.Eventually(t, func() bool {
		return providerCalls.Load() >= 2
	}, 10*time.Second, 100*time.Millisecond,
		"Finalizer's detached run should have executed orphaned call "+
			"(provider call count >= 2, got %d)", providerCalls.Load())

	// CRITICAL VERIFICATION 4: The orphaned call's prompt appears in message history.
	// This proves the call was actually executed, not just queued.
	//
	// The finalizer's detached run should have completed at least one turn for the orphaned call,
	// which means the prompt should appear in the session's message history.
	require.Eventually(t, func() bool {
		msgs, err := messages.List(t.Context(), sess.ID)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			for _, part := range m.Parts {
				if tc, ok := part.(message.TextContent); ok {
					if tc.Text == "orphaned call" {
						return true
					}
				}
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond,
		"Orphaned call prompt should appear in message history "+
			"(proving the call was actually executed, not just queued)")

	// Allow time for the blocked cleanup goroutine to finish before test completes.
	// The defer above will unblock it and restore the original function.
	time.Sleep(3 * time.Second)

	// Clean up the DB connection so tmpDir can be removed. db.Connect pools
	// connections by absolute dataDir path and additionally opens a
	// separate read-only pool sharing the same refcount (see
	// internal/db/connect.go) — a raw conn.Close() only closes the writer
	// handle and leaves the reader pool's file handle open, which on
	// Windows blocks t.TempDir()'s own RemoveAll cleanup. db.Release tears
	// down both.
	require.NoError(t, db.Release(tmpDir))
}

// p0338PumpCoordinator adapts session.SessionAgentCallData to agent.SessionAgentCall
// and executes it through the exported agent.SessionAgent interface, mirroring
// production's coordinatorAdapterImpl (internal/app/app.go) without needing the
// unexported *coordinator/*sessionAgent types this external test package can't see.
type p0338PumpCoordinator struct {
	sessionAgent agent.SessionAgent
}

func (p *p0338PumpCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	call := agent.FromSessionAgentCallData(callData)
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
