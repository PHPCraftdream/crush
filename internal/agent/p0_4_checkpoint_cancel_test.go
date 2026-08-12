package agent

// P0-4 follow-up (docs/reviews/2026-08-12-post-fix-release-readiness-review.md):
// found during independent zero-trust review of the delegated fix. The
// delegated diff added a cancelable writeCtx/writeCancel pair for the
// checkpoint goroutine's DB write, but stopCheckpoint (the function actually
// called when a turn is ending) never held a reference to writeCancel and
// therefore never called it — the "cancelable context" was cancelable in
// name only. stopCheckpoint's own comment claimed "This allows stopCheckpoint
// to actually cancel an in-flight Update", which was false: writeCancel was
// only ever invoked via `defer writeCancel()` INSIDE the goroutine itself,
// which cannot fire until the goroutine's own blocking Update call already
// returned -- a circular dependency providing zero protection against an
// actual hang. The delegated diff's own regression test for this
// (TestCheckpointContextCancellation_P0_4Regression) was fully vacuous: its
// body contains no assertions or even any function calls, only comments
// describing the mechanism "by code inspection."
//
// Fixed by storing writeCancel on the outer scope (checkpointWriteCancel)
// and having stopCheckpoint call it immediately after signaling stop, before
// waiting out its own 5s grace.
//
// This test proves the fix with a genuinely blocked checkpoint write: a
// message-service wrapper blocks Update calls that carry a Partial finish
// (the checkpoint write's own signature) until either explicitly unblocked
// or its context is cancelled, and records which happened. A model that
// streams some content then pauses mid-turn gives the checkpoint ticker a
// window to fire and get stuck in that blocked Update. CancelAll() is then
// invoked while the write is still genuinely blocked; the test asserts the
// blocked Update's context was cancelled (not that it merely returned
// eventually on its own).
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go's stopCheckpoint, remove the `if checkpointWriteCancel !=
//     nil { checkpointWriteCancel() ... }` block.
//  2. Run: go test ./internal/agent -run TestP0_4_StopCheckpointCancelsBlockedWrite -v
//  3. FAIL: the test's wait for ctxCanceledCh times out (5s) because nothing
//     ever cancels writeCtx -- the blocked Update call is only released when
//     this test manually closes releaseCh as a cleanup fallback, proving the
//     write was never actually cancelled by stopCheckpoint.
//  4. Restore the checkpointWriteCancel call.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// checkpointBlockingMessages wraps a real message.Service and blocks any
// Update call whose message carries a Partial finish (the checkpoint
// write's own signature — see runTurn's checkpoint goroutine, which always
// calls AddFinish(..., Partial: true) before writing). Non-checkpoint
// writes (terminal finishes, tool results, etc.) pass through untouched so
// the rest of a real turn can proceed normally.
type checkpointBlockingMessages struct {
	message.Service
	mu            sync.Mutex
	armed         bool
	updateStarted chan struct{} // closed once a checkpoint Update call is blocked
	releaseCh     chan struct{} // test closes this to unblock manually, as a cleanup fallback
	ctxCanceledCh chan struct{} // closed if the blocked call observed ctx.Done() first
	startOnce     sync.Once
}

func newCheckpointBlockingMessages(inner message.Service) *checkpointBlockingMessages {
	return &checkpointBlockingMessages{
		Service:       inner,
		updateStarted: make(chan struct{}),
		releaseCh:     make(chan struct{}),
		ctxCanceledCh: make(chan struct{}),
	}
}

func (m *checkpointBlockingMessages) arm() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.armed = true
}

func (m *checkpointBlockingMessages) Update(ctx context.Context, msg message.Message) error {
	m.mu.Lock()
	armed := m.armed
	m.mu.Unlock()

	fp := msg.FinishPart()
	isCheckpointWrite := armed && fp != nil && fp.Partial

	if isCheckpointWrite {
		m.startOnce.Do(func() { close(m.updateStarted) })
		select {
		case <-ctx.Done():
			close(m.ctxCanceledCh)
			return ctx.Err()
		case <-m.releaseCh:
			// Fell through to the real write below.
		}
	}

	return m.Service.Update(ctx, msg)
}

// newPausingSSEModel returns a model whose streamed response sends an
// initial content chunk immediately (so currentAssistant.Parts becomes
// non-empty, giving the checkpoint ticker something to write), then blocks
// on pauseCh before sending the final chunk -- keeping the turn open long
// enough for at least one checkpoint tick to fire.
func newPausingSSEModel(t *testing.T, content string, pauseCh <-chan struct{}) Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		early := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, content),
		}
		for _, c := range early {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		<-pauseCh // held open until the test releases it
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		if fl != nil {
			fl.Flush()
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
	return Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
}

// TestP0_4_StopCheckpointCancelsBlockedWrite proves that CancelAll (which
// drives the turn's cancel path, which calls stopCheckpoint on the way out)
// genuinely cancels a checkpoint write that is still blocked mid-flight,
// rather than only giving up waiting on it.
func TestP0_4_StopCheckpointCancelsBlockedWrite(t *testing.T) {
	env := testEnv(t)
	pauseCh := make(chan struct{})
	model := newPausingSSEModel(t, "partial content for checkpoint", pauseCh)

	blockingMsgs := newCheckpointBlockingMessages(env.messages)

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             blockingMsgs,
		DisableAutoSummarize: true,
		CheckpointInterval:   10 * time.Millisecond,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p0-4-checkpoint-cancel")
	require.NoError(t, err)

	// Arm blocking only after the session exists, so setup writes (if any)
	// pass through freely.
	blockingMsgs.arm()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = sessionAgent.Run(ctx, SessionAgentCall{
			SessionID: sess.ID,
			Prompt:    "say something",
		})
	}()

	// Wait for the checkpoint goroutine to actually be blocked inside Update.
	select {
	case <-blockingMsgs.updateStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("checkpoint write never started — the ticker did not fire, or Partial detection is broken")
	}

	// The write is now genuinely stuck. Drive the turn's cancel path.
	start := time.Now()
	sessionAgent.CancelAll()

	select {
	case <-blockingMsgs.ctxCanceledCh:
		elapsed := time.Since(start)
		t.Logf("blocked checkpoint write observed ctx cancellation after %v", elapsed)
		require.Less(t, elapsed, 5*time.Second, "stopCheckpoint should cancel the blocked write promptly, not after its own 5s grace")
	case <-time.After(5 * time.Second):
		t.Fatal("blocked checkpoint write was never cancelled — checkpointWriteCancel is not being invoked by stopCheckpoint")
	}

	// Cleanup: release the SSE pause and the message-service block so the
	// background Run() goroutine and HTTP handler can exit.
	close(pauseCh)
	close(blockingMsgs.releaseCh)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not exit after cleanup unblock")
	}
}
