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
	"github.com/stretchr/testify/require"
)

// TestP2_3_HTTPProviderRespectsCancellation verifies that HTTP-based providers
// (using openaicompat.New) respect context cancellation and return promptly when
// the context is canceled, even if the provider stream hangs indefinitely.
//
// This test covers the HTTP-based provider category. CLI-based providers are
// already covered by tests in internal/agent/cliprovider/provider_test.go:
// - TestStreamKillUsesTreeKillStillTerminatesChild
// - TestStreamWaitBoundedOnGrandchildHoldsStderr
//
// Provider categories in this fork:
//   - HTTP-based providers (openaicompat pattern) - covered by this test
//   - CLI-based providers (cliprovider pattern) - covered by cliprovider tests
//   - Other providers (anthropic, bedrock, google, openai, openrouter, vercel) -
//     all use HTTP streaming under the hood and should behave similarly to
//     openaicompat, but are not explicitly tested here.
//
// The test creates a mock SSE server that sends some initial chunks, then hangs
// (never sends DONE, never closes connection). It then verifies that:
// 1. agent.Stream returns within bounded time (< 5 seconds) after cancellation
// 2. The error is context.Canceled or related to cancellation
//
// REVERT CHECK PROCEDURE:
//  1. In openaicompat implementation, find the streaming loop and remove
//     context.Done() checks (or make them ignore the context).
//  2. Run: go test ./internal/agent -run TestP2_3_HTTPProviderRespectsCancellation -v
//  3. The test will FAIL because agent.Stream will hang until the 5s test timeout
//  4. Restore the context check and the test will PASS again.
func TestP2_3_HTTPProviderRespectsCancellation(t *testing.T) {
	t.Parallel()

	var streamStarted atomic.Bool
	var chunksSent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamStarted.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Send some initial chunks to prove the stream is working.
		initialChunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		}
		for _, c := range initialChunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
			chunksSent.Add(1)
		}

		// Hang forever - never send DONE, never close connection.
		// The test will cancel the context and expect the stream to exit.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// Build a provider using openaicompat (HTTP-based).
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
		ModelCfg: config.SelectedModel{
			Provider: "openaicompat",
			Model:    "probe",
		},
	}

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a test agent",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})

	sess, err := env.sessions.Create(context.Background(), "p2-3 http provider test")
	require.NoError(t, err)

	// Create a simple user message to start the stream.
	_, err = env.messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err)

	// Create a cancellable context with a short timeout.
	// The test expects the provider to exit within 5s after cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start streaming in a goroutine.
	streamDone := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "test",
			MaxOutputTokens: 1000,
		})
		streamDone <- err
	}()

	// Wait for the stream to start and send initial chunks.
	timeout := time.After(2 * time.Second)
	for !streamStarted.Load() || chunksSent.Load() < 2 {
		select {
		case <-timeout:
			t.Fatal("timeout waiting for stream to start and send initial chunks")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Now cancel the context after confirming the stream is working.
	cancel()

	// The stream should exit quickly (< 5s) and return a cancellation error.
	// If the provider doesn't respect cancellation, this will hang until the
	// outer test timeout.
	select {
	case err := <-streamDone:
		// Verify the error is related to cancellation.
		require.Error(t, err, "stream should return an error after cancellation")
		isCanceled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		require.True(t, isCanceled, "error should be context.Canceled or context.DeadlineExceeded, got: %T: %v", err, err)
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not exit within 5 seconds after context cancellation - provider is not respecting cancellation")
	}

	// Verify that some chunks were sent (the test setup is valid).
	require.GreaterOrEqual(t, chunksSent.Load(), int64(2), "stream should have sent initial chunks before hanging")
}

// TestP2_3_ManualCompactionWatchdogCatchesIdleStall verifies that manual/standalone
// compaction (runSummarize path) has a watchdog that catches idle stalls significantly
// earlier than the 10-minute hard timeout.
//
// Without this (P2-3 problem 1 from the 2026-08-07 concurrency review), a provider
// that stops sending data during manual /compact would appear "working" for the full
// 10 minutes (or longer if it ignores cancel), because notifyWatchdog(ctx) calls
// inside runSummarizeBody/runSummarizeSilent were no-ops (no watchdog installed).
//
// This test injects a short streamIdleTimeout and overrides the test seam for
// streamWatchdogTick to make the test fast. It creates a mock SSE server that sends
// initial chunks then hangs, starts a manual Summarize, and verifies that the
// watchdog fires (context canceled) well before the 10-minute genCtx timeout would
// have expired.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go:~3059-3107, comment out the startStreamWatchdog call:
//     // BUG: No watchdog for manual compaction (P2-3 problem 1 revert check)
//     // wd := startStreamWatchdog(...)
//     // genCtx = withWatchdogBump(genCtx, wd.bump)
//     // defer func() { <-wd.done }()
//  2. Run: go test ./internal/agent -run TestP2_3_ManualCompactionWatchdogCatchesIdleStall -v
//  3. The test will FAIL (timeout) because the Summarize waits for the full 10-minute genCtx
//  4. Restore the watchdog and the test will PASS again.
func TestP2_3_ManualCompactionWatchdogCatchesIdleStall(t *testing.T) {
	t.Parallel()

	var streamStarted atomic.Bool
	var chunksSent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamStarted.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Send some initial chunks to prove the stream is working.
		initialChunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"summary"}}]}`,
		}
		for _, c := range initialChunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
			chunksSent.Add(1)
		}

		// Hang forever - never send DONE, never close connection.
		// The watchdog should catch this idle stall.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// Build a provider using openaicompat (HTTP-based).
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
		ModelCfg: config.SelectedModel{
			Provider: "openaicompat",
			Model:    "probe",
		},
	}

	// Override the test seam for fast watchdog behavior.
	shortTick := 100 * time.Millisecond
	oldTick := testStreamWatchdogTick
	testStreamWatchdogTick = func() time.Duration { return shortTick }
	defer func() { testStreamWatchdogTick = oldTick }()

	env := testEnv(t)
	// Inject a SHORT streamIdleTimeout so the test runs fast.
	// The real production default is 10 minutes, but that would make the test too slow.
	shortIdleTimeout := 500 * time.Millisecond
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a test agent",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		StreamIdleTimeout:    shortIdleTimeout,
	})

	sess, err := env.sessions.Create(context.Background(), "p2-3 manual compaction watchdog test")
	require.NoError(t, err)

	// Create enough messages to trigger summarization (runSummarizeBody needs messages to compact).
	// We need at least 1 message (runSummarizeBody returns nil if len(msgs) == 0).
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

	// Start manual Summarize (the path being tested).
	// This calls runSummarize, which should now have a watchdog.
	summarizeDone := make(chan error, 1)
	go func() {
		summarizeDone <- a.Summarize(context.Background(), sess.ID, fantasy.ProviderOptions{})
	}()

	// Wait for the stream to start and send initial chunks.
	timeout := time.After(2 * time.Second)
	for !streamStarted.Load() || chunksSent.Load() < 2 {
		select {
		case <-timeout:
			t.Fatal("timeout waiting for summarize stream to start and send initial chunks")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The watchdog should fire due to idle stall (provider stopped sending chunks).
	// With shortIdleTimeout (500ms) and shortTick (100ms), it should fire within ~1 second.
	// This is MUCH faster than the 10-minute genCtx timeout would have been.
	//
	// If the watchdog is missing (P2-3 problem 1), Summarize will wait for the full 10-minute
	// genCtx timeout, causing this test to timeout/fail.
	select {
	case err := <-summarizeDone:
		// Summarize should return an error (watchdog fired or stream error).
		// The exact error format may vary, but it should be non-nil.
		require.Error(t, err, "Summarize should return an error when watchdog fires due to idle stall")
	case <-time.After(10 * time.Second):
		t.Fatal("Summarize did not return within 10 seconds - watchdog is not catching idle stall (P2-3 problem 1)")
	}

	// Verify that some chunks were sent (the test setup is valid).
	require.GreaterOrEqual(t, chunksSent.Load(), int64(2), "summarize stream should have sent initial chunks before hanging")
}
