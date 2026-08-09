package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// blockingTool is a fantasy.AgentTool whose Run() blocks on a channel that
// the test controls, WITHOUT selecting on ctx.Done(). This deliberately
// simulates the "non-cooperative" class of work task #343 worries about:
// tool/turn code that doesn't respect context cancellation, so cancelling
// the generation context alone is not enough to make Run() return promptly.
type blockingTool struct {
	started chan struct{}
	unblock chan struct{}
}

func (b *blockingTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "blocking_tool"}
}

func (b *blockingTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	close(b.started)
	<-b.unblock // deliberately ignores ctx — the whole point of this test
	return fantasy.NewTextResponse("unblocked"), nil
}

func (b *blockingTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (b *blockingTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

// TestP343_CancelAllTrueJoinWaitsForRealBlockedRun is the end-to-end
// regression test for task #343's Дефект 1: CancelAll must genuinely join on
// live Run() goroutines via runWg, not merely poll IsBusy() (a proxy signal
// that can go idle before the actual goroutine has unwound).
//
// The mock-based test added by the delegated round
// (internal/app/p1_3_shutdown_db_test.go) hardcodes CancelAll's return value
// and never exercises the real runWg/Run() wiring at all — this test closes
// that gap with a REAL sessionAgent.Run() call that is deliberately stuck
// inside a tool call ignoring context cancellation, exactly the scenario the
// runWg join exists to detect.
//
// This test FAILS (or, worse, silently proves nothing) if runWg.Add/Done are
// not correctly wired around every Run() call: CancelAll would then return
// almost instantly (stillBusy possibly wrong) instead of genuinely blocking
// for close to the 5-second grace period while the tool is stuck.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go's CancelAll, revert to the old IsBusy() polling loop (or
//     just remove the a.runWg.Add(1)/defer a.runWg.Done() pair from Run()).
//  2. Run: go test ./internal/agent -run TestP343_CancelAllTrueJoinWaitsForRealBlockedRun -v
//  3. Observe the timing/return-value assertions no longer hold the same way.
//  4. Restore the fix.
func TestP343_CancelAllTrueJoinWaitsForRealBlockedRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"blocking_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
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
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	tool := &blockingTool{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{tool},
		DisableAutoSummarize: true,
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p343-cancelall-join-test")
	require.NoError(t, err)
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = sa.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "trigger the blocking tool"})
	}()

	// Wait for the tool to actually be invoked — Run() is now genuinely
	// stuck inside code that ignores context cancellation.
	select {
	case <-tool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking tool never started — test setup is broken, proves nothing")
	}

	// CancelAll must now genuinely wait close to its 5s grace period (the
	// tool call cannot be interrupted) and report stillBusy=true — NOT
	// return almost instantly. This is the core assertion: a true
	// runWg.Wait() join blocks on the real goroutine, not a mailbox-state
	// proxy that could already look idle.
	cancelStart := time.Now()
	stillBusy := sa.CancelAll()
	cancelElapsed := time.Since(cancelStart)

	require.True(t, stillBusy, "CancelAll must report stillBusy=true while the tool call is still genuinely blocked")
	require.GreaterOrEqual(t, cancelElapsed, 4*time.Second,
		"CancelAll must have genuinely waited close to its 5s grace period for the real Run() goroutine, not returned early on a stale state read; got %v", cancelElapsed)

	// Now unblock the tool so the stuck Run() goroutine can finally finish
	// and this test can clean up without leaking a goroutine.
	close(tool.unblock)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() never returned after unblocking the tool — leaked goroutine")
	}
}
