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
	"github.com/stretchr/testify/require"
)

// TestProbe_CheckpointStopDoesNotStallStep is a temporary probe for the
// review claim that stopCheckpoint() blocks for its full 5s timeout on
// every OnStepFinish, because the checkpoint goroutine only exits on
// genCtx.Done() and genCtx stays alive for the whole body of Run().
func TestProbe_CheckpointStopDoesNotStallStep(t *testing.T) {
	// Single-step, text-only completion: enough to make OnTextDelta fire
	// (which starts the checkpoint ticker) and then OnStepFinish run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
			// Spread the deltas out so the 100ms checkpoint ticker
			// definitely fires at least once mid-stream.
			time.Sleep(150 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	env := testEnv(t)
	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		CheckpointInterval:   100 * time.Millisecond,
		DisableAutoSummarize: true,
	})

	sess, err := env.sessions.Create(t.Context(), "probe session")
	require.NoError(t, err)

	start := time.Now()
	_, err = agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hi",
		SessionID:       sess.ID,
		MaxOutputTokens: 1000,
	})
	elapsed := time.Since(start)
	require.NoError(t, err)

	t.Logf("PROBE: single-step run took %s (server-side stream is ~450ms)", elapsed)
	require.Less(t, elapsed, 2*time.Second,
		"Run stalled far beyond the ~450ms stream: checkpoint lifecycle is blocking on its 5s timeout")
}

type probeToolParams struct{}

// TestProbe_CheckpointStallIsPerStep drives a two-step run (tool call, then
// final text) to establish whether the stall is once-per-turn or once-per-step.
func TestProbe_CheckpointStallIsPerStep(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		var chunks []string
		if n == 1 {
			chunks = []string{
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"let me check"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"probe_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
			}
		} else {
			chunks = []string{
				`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"all done"},"finish_reason":null}]}`,
				`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`,
			}
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(150 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	probeTool := fantasy.NewAgentTool(
		"probe_tool",
		"a trivial probe tool that returns instantly",
		func(ctx context.Context, p probeToolParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		},
	)

	env := testEnv(t)
	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	agent := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{probeTool},
		CheckpointInterval:   100 * time.Millisecond,
		DisableAutoSummarize: true,
	})

	sess, err := env.sessions.Create(t.Context(), "probe session 2")
	require.NoError(t, err)

	start := time.Now()
	_, err = agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hi",
		SessionID:       sess.ID,
		MaxOutputTokens: 1000,
	})
	elapsed := time.Since(start)
	require.NoError(t, err)

	t.Logf("PROBE: two-step run took %s across %d provider calls (server-side stream is ~750ms)", elapsed, calls.Load())
	require.Less(t, elapsed, 2*time.Second, "two-step run stalled")
}
