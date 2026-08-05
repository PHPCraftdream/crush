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
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestRunSummarizeSilent_CallsActivityNotify verifies that runSummarizeSilent
// calls notifyActivity on its stream callbacks, keeping the session's heartbeat
// alive during long-running compaction (task #310).
func TestRunSummarizeSilent_CallsActivityNotify(t *testing.T) {
	t.Parallel()

	// Track whether notifyActivity was called.
	var activityCalled atomic.Bool
	// Create a context with a mocked notifyActivity callback.
	ctx := context.Background()
	ctx = context.WithValue(ctx, activityNotifyContextKey{}, func() {
		activityCalled.Store(true)
	})

	// Build a test agent using the existing pattern.
	env := testEnv(t)
	var calls atomic.Int64
	srv := summarizeSSEServer(5, 2, &calls)
	t.Cleanup(srv.Close)
	lm := languageModelFromServer(t, srv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sa := a.(*sessionAgent)

	// Create a session with enough messages to trigger summarization.
	sess, err := env.sessions.Create(t.Context(), "summarize activity test")
	require.NoError(t, err)
	// Create 8 messages (4 user + 4 assistant), len >= 4 triggers summary
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Run the silent summarize.
	err = sa.runSummarizeSilent(ctx, sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err)

	// Verify that the provider was called (the summary stream actually ran).
	require.Equal(t, int64(1), calls.Load(), "summary stream should have been invoked")

	// Verify that notifyActivity was called during the stream.
	require.True(t, activityCalled.Load(), "notifyActivity should have been called during summarize stream")
}

// TestRunSummarizeSilent_CallsWatchdogBump verifies that runSummarizeSilent
// calls notifyWatchdog on its stream callbacks, reporting progress to the
// watchdog so healthy long-running compaction doesn't get falsely killed as
// "no provider activity" (task #310).
func TestRunSummarizeSilent_CallsWatchdogBump(t *testing.T) {
	t.Parallel()

	// Track how many times the watchdog bump was called.
	var bumpCount atomic.Int32
	// Create a context with a mocked watchdog bump callback.
	ctx := context.Background()
	ctx = context.WithValue(ctx, watchdogBumpContextKey{}, func() {
		bumpCount.Add(1)
	})

	// Build a test agent using the existing pattern.
	env := testEnv(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Stream multiple text deltas to exercise OnTextDelta repeatedly.
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"s"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"u"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"m"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"m"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"r"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"y"},"finish_reason":null}]}`,
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`),
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
			// Small delay to ensure callbacks fire.
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	lm := languageModelFromServer(t, srv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sa := a.(*sessionAgent)

	// Create a session with enough messages to trigger summarization.
	sess, err := env.sessions.Create(t.Context(), "summarize watchdog test")
	require.NoError(t, err)
	// Create 8 messages (4 user + 4 assistant), len >= 4 triggers summary
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Run the silent summarize.
	err = sa.runSummarizeSilent(ctx, sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err)

	// Verify that the provider was called (the summary stream actually ran).
	require.Equal(t, int64(1), calls.Load(), "summary stream should have been invoked")

	// Verify that the watchdog bump was called multiple times (once per text delta).
	require.Greater(t, bumpCount.Load(), int32(5), "watchdog bump should have been called multiple times during stream")
}

// TestRunSummarizeBody_CallsActivityNotify verifies that runSummarizeBody
// calls notifyActivity on its stream callbacks, keeping the session's heartbeat
// alive during long-running compaction (task #310).
func TestRunSummarizeBody_CallsActivityNotify(t *testing.T) {
	t.Parallel()

	// Track whether notifyActivity was called.
	var activityCalled atomic.Bool
	// Create a context with a mocked notifyActivity callback.
	ctx := context.Background()
	ctx = context.WithValue(ctx, activityNotifyContextKey{}, func() {
		activityCalled.Store(true)
	})

	// Build a test agent using the existing pattern.
	env := testEnv(t)
	var calls atomic.Int64
	srv := summarizeSSEServer(5, 2, &calls)
	t.Cleanup(srv.Close)
	lm := languageModelFromServer(t, srv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sa := a.(*sessionAgent)

	// Create a session with enough messages to trigger summarization.
	sess, err := env.sessions.Create(t.Context(), "summarize body activity test")
	require.NoError(t, err)
	// Create 8 messages (4 user + 4 assistant), len >= 4 triggers summary
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Run the summarize body.
	err = sa.runSummarizeBody(ctx, sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err)

	// Verify that the provider was called (the summary stream actually ran).
	require.Equal(t, int64(1), calls.Load(), "summary stream should have been invoked")

	// Verify that notifyActivity was called during the stream.
	require.True(t, activityCalled.Load(), "notifyActivity should have been called during summarize stream")
}

// TestRunSummarizeBody_CallsWatchdogBump verifies that runSummarizeBody
// calls notifyWatchdog on its stream callbacks, reporting progress to the
// watchdog so healthy long-running compaction doesn't get falsely killed as
// "no provider activity" (task #310).
func TestRunSummarizeBody_CallsWatchdogBump(t *testing.T) {
	t.Parallel()

	// Track how many times the watchdog bump was called.
	var bumpCount atomic.Int32
	// Create a context with a mocked watchdog bump callback.
	ctx := context.Background()
	ctx = context.WithValue(ctx, watchdogBumpContextKey{}, func() {
		bumpCount.Add(1)
	})

	// Build a test agent using the existing pattern.
	env := testEnv(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Stream multiple text deltas to exercise OnTextDelta repeatedly.
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"s"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"u"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"m"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"m"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"r"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"y"},"finish_reason":null}]}`,
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`),
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
			// Small delay to ensure callbacks fire.
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	lm := languageModelFromServer(t, srv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sa := a.(*sessionAgent)

	// Create a session with enough messages to trigger summarization.
	sess, err := env.sessions.Create(t.Context(), "summarize body watchdog test")
	require.NoError(t, err)
	// Create 8 messages (4 user + 4 assistant), len >= 4 triggers summary
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Run the summarize body.
	err = sa.runSummarizeBody(ctx, sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err)

	// Verify that the provider was called (the summary stream actually ran).
	require.Equal(t, int64(1), calls.Load(), "summary stream should have been invoked")

	// Verify that the watchdog bump was called multiple times (once per text delta).
	require.Greater(t, bumpCount.Load(), int32(5), "watchdog bump should have been called multiple times during stream")
}
