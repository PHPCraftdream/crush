package agent

// P0-4 regression test (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// CancelAll() must genuinely join on every session-scoped execution unit
// that can write to the DB — not just Run() calls. Before this fix, manual
// Summarize() calls and a title-generation goroutine that outlived
// titleJoinGrace were never registered in runWg, so CancelAll's
// runWg.Wait() could report stillBusy=false (and let App.Shutdown close the
// DB) while either was still genuinely in flight and could still write.
//
// SCOPE NOTE (added on independent review): the delegated /crush fix's own
// version of these two tests (TestP0_4_SummarizeTracksRunWg,
// TestP0_4_TitleTracksRunWg) waited for Summarize()/Run() to fully
// complete BEFORE ever calling CancelAll() — at that point runWg is
// trivially empty whether or not the fix is present, since the goroutine
// has already finished and decremented (or, pre-fix, was simply never
// counted and the wg was just as empty). Reverting the fix and re-running
// those two tests confirmed they PASS unchanged either way — the fourth
// occurrence of a vacuous test in this babygoal round. Rewritten below to
// call CancelAll() WHILE Summarize()/the title goroutine are still
// genuinely blocked mid-flight (mirroring the proven technique in
// p343_cancelall_join_test.go's blockingTool, adapted to the HTTP
// transport layer since manual compaction and title generation don't go
// through the tool-call path).
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go's Summarize, remove a.runWg.Add(1) and defer a.runWg.Done().
//  2. In agent.go's runTurn title-goroutine spawn (~line 1817), remove
//     a.runWg.Add(1) before go func() and defer a.runWg.Done() inside it.
//  3. Run: go test ./internal/agent -run TestP0_4_CancelAll -v
//  4. FAIL: both tests' `require.True(t, stillBusy, ...)` and
//     `require.GreaterOrEqual(t, elapsed, ...)` assertions fail — CancelAll
//     returns almost instantly with stillBusy=false, because runWg never
//     counted the still-blocked goroutine.
//  5. Restore both Add/Done pairs.

import (
	"context"
	"errors"
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

// testTitleJoinGrace/testCancelAllGrace override the production 5s
// titleJoinGrace/CancelAll-grace constants for this file's and
// p343_cancelall_join_test.go's tests via SessionAgentOptions
// (task #454, following up on task #450's test-speed investigation).
// 1s is a genuine 5x speedup, not a "smallest value that still barely
// passes" one — see task #445/M7's history in this codebase for why a
// wide margin is deliberately preferred over the tightest one that
// happens to pass locally.
//
// testCancelAllGraceLowerBound is the proportional equivalent of the
// original "4 of 5 real seconds" lower-bound assertion (80% of grace),
// rounded down slightly (700ms of 1s, not 800ms) to leave a bit more
// slack for scheduling jitter at this much smaller absolute scale.
const (
	testTitleJoinGrace           = 1 * time.Second
	testCancelAllGrace           = 1 * time.Second
	testCancelAllGraceLowerBound = 700 * time.Millisecond
)

// blockingHTTPClient is an option.HTTPClient (Do(*http.Request)
// (*http.Response, error)) that blocks on a channel the test controls,
// WITHOUT selecting on req.Context() — the transport-layer equivalent of
// p343_cancelall_join_test.go's blockingTool. This deliberately defeats
// context-based cancellation (genCtx/titleCtx firing does not unblock it),
// so a caller waiting on the goroutine that's stuck here is genuinely
// testing whether something joins on the real goroutine, not merely on
// how fast context cancellation propagates.
type blockingHTTPClient struct {
	startedOnce sync.Once
	started     chan struct{}
	unblock     chan struct{}
}

func (b *blockingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	b.startedOnce.Do(func() { close(b.started) })
	<-b.unblock // deliberately ignores req.Context()
	return nil, errors.New("p0-4 test: blocking client unblocked")
}

func newBlockingHTTPClientModel(t *testing.T) (Model, *blockingHTTPClient) {
	t.Helper()
	client := &blockingHTTPClient{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL("http://127.0.0.1:1"), // never dialed — Do() is intercepted
		openaicompat.WithAPIKey("probe"),
		openaicompat.WithHTTPClient(client),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	return Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}, client
}

func newFastSSEModel(t *testing.T, content string) Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, content),
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

// TestP0_4_CancelAllJoinsBlockedSummarize proves CancelAll() genuinely
// waits for a real, still-in-flight Summarize() call before reporting
// stillBusy — not merely until Run()-tracked work drains.
func TestP0_4_CancelAllJoinsBlockedSummarize(t *testing.T) {
	t.Parallel()
	model, client := newBlockingHTTPClientModel(t)

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
		// testCancelAllGrace (task #454, following up on task #450's
		// test-speed investigation), not the production 5s default — see
		// the constant's own doc comment for the margin rationale.
		CancelAllGrace: testCancelAllGrace,
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p0-4-cancelall-summarize-test")
	require.NoError(t, err)
	for i := range 3 {
		_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	summarizeDone := make(chan struct{})
	go func() {
		defer close(summarizeDone)
		snapshot := &SummarizeSnapshot{model: model, promptPrefix: "summarize this"}
		_ = sa.Summarize(ctx, sess.ID, snapshot)
	}()

	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Summarize() never reached the blocking provider call — test setup is broken, proves nothing")
	}

	cancelStart := time.Now()
	stillBusy := sa.CancelAll()
	cancelElapsed := time.Since(cancelStart)

	require.True(t, stillBusy, "CancelAll must report stillBusy=true while Summarize() is still genuinely blocked on its provider call")
	require.GreaterOrEqual(t, cancelElapsed, testCancelAllGraceLowerBound,
		"CancelAll must have genuinely waited close to its grace period for the real Summarize() goroutine, not returned early because it was never tracked in runWg; got %v", cancelElapsed)

	close(client.unblock)
	select {
	case <-summarizeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Summarize() never returned after unblocking — leaked goroutine")
	}
}

// TestP0_4_CancelAllJoinsAbandonedTitleGoroutine proves CancelAll() waits
// for a title-generation goroutine that outlived titleJoinGrace and was
// abandoned by Run() (Run() itself already returned) — not just work still
// tracked by the Run() call that spawned it.
func TestP0_4_CancelAllJoinsAbandonedTitleGoroutine(t *testing.T) {
	t.Parallel()
	titleModel, client := newBlockingHTTPClientModel(t)
	turnModel := newFastSSEModel(t, "hello back")

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:           turnModel,
		SmallModel:           titleModel, // generateTitle tries the small model first
		SystemPrompt:         "you are a probe",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
		// testTitleJoinGrace/testCancelAllGrace (task #454) — see their doc
		// comments for the margin rationale.
		TitleJoinGrace: testTitleJoinGrace,
		CancelAllGrace: testCancelAllGrace,
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p0-4-cancelall-title-test")
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		// First turn (no prior messages) triggers title generation.
		_, _ = sa.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "hello"})
	}()

	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("title generation never reached the blocking provider call — test setup is broken, proves nothing")
	}

	// The main turn (fast SSE model) finishes quickly, but runTurn's own
	// deferred join only waits up to testTitleJoinGrace for the still-blocked
	// title goroutine before giving up and letting Run() return — Run()
	// abandons it exactly as P0-4 describes.
	select {
	case <-runDone:
	case <-time.After(testTitleJoinGrace + 3*time.Second):
		t.Fatal("Run() never returned — expected it to abandon the blocked title goroutine after testTitleJoinGrace")
	}

	// At this point Run()'s own runWg.Add/Done pair has already resolved.
	// The title goroutine is STILL blocked in client.Do(). CancelAll must
	// still see it as live.
	cancelStart := time.Now()
	stillBusy := sa.CancelAll()
	cancelElapsed := time.Since(cancelStart)

	require.True(t, stillBusy, "CancelAll must report stillBusy=true while the abandoned title goroutine is still genuinely blocked")
	require.GreaterOrEqual(t, cancelElapsed, testCancelAllGraceLowerBound,
		"CancelAll must have genuinely waited close to its own grace period for the abandoned title goroutine, not returned early because Run() had already finished; got %v", cancelElapsed)

	// Unblock so the goroutine can finish and the test doesn't leak it.
	close(client.unblock)
}
