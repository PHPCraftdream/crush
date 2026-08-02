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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangingSSEServer returns an httptest.Server whose handler blocks until
// EITHER the request's own context is cancelled (the client gave up — the
// case this test drives) OR the returned release channel is closed, without
// ever writing a response. It stands in for a wedged provider connection: a
// TCP connection that never closes and a stream that never emits a single
// byte, which is exactly the shape a.generateTitle's unbounded agent.Stream
// call has no timeout of its own to escape.
//
// The release channel exists purely for test cleanup: httptest.Server.Close
// blocks until every in-flight handler goroutine has returned (it does its
// own internal sync.WaitGroup.Wait), and relying solely on r.Context().Done
// to unblock the handler raced Close() itself in practice — closing the
// listener does not necessarily fire the still-open connection's request
// context promptly, so t.Cleanup(srv.Close) could (and, observed once while
// developing this test, did) hang for the rest of the test binary's
// lifetime. Closing release before calling Close() gives the handler a
// second, deterministic way out that doesn't depend on that race.
func hangingSSEServer(release <-chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
}

// TestRun_TitleProviderHangs_DoesNotBlockRunForever is the regression test
// for the title-generation hang: before this fix, the background title
// goroutine launched from runTurn (needsTitle's wg.Go call) ran
// a.generateTitle on titleCtx := ctx — the raw, potentially unbounded ctx
// passed into runTurn from its caller (e.g. context.Background() from
// `crush run`) — NOT genCtx, the per-turn context the stream watchdog
// cancels. generateTitle's two model attempts (small then large) are each a
// blocking agent.Stream call with no timeout of their own. If the title
// provider hangs (dead TCP connection, provider never closes the stream),
// that goroutine never returns, and the deferred `wg.Wait()` in runTurn —
// which the whole Run() call chain waits on — blocks forever, even though
// the main turn's own stream completed successfully and even though the
// stream watchdog correctly cancelled genCtx for the main turn's own
// purposes.
//
// This test drives Run() with a working large-model SSE server (completes
// normally) and a title (small-model) SSE server whose handler blocks
// forever without writing a byte. It sets SessionAgentOptions.
// TitleGenerationMaxDuration on this agent instance so the backstop timeout
// fires quickly instead of the test needing to wait out a production-sized
// bound, and asserts Run() still returns within a generous wall-clock budget
// with a successful main-turn result — proving the best-effort, cosmetic
// title generation can never hold up the turn it rides along with.
func TestRun_TitleProviderHangs_DoesNotBlockRunForever(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	srv := singleTurnSSEServer(nil)
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	release := make(chan struct{})
	titleSrv := hangingSSEServer(release)
	// t.Cleanup runs LIFO, so registering close(release) AFTER
	// titleSrv.Close means close(release) actually runs FIRST: the handler
	// is released unconditionally before Close() starts waiting on it, so
	// Close() can never hang regardless of whether r.Context().Done() would
	// have fired promptly enough on its own. See hangingSSEServer's doc
	// comment.
	t.Cleanup(titleSrv.Close)
	t.Cleanup(func() { close(release) })
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	smallModel := Model{
		Model:      titleLM,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:                 model,
		SmallModel:                 smallModel,
		SystemPrompt:               "you are a probe",
		IsYolo:                     true,
		Sessions:                   env.sessions,
		Messages:                   env.messages,
		Tools:                      []fantasy.AgentTool{},
		DisableAutoSummarize:       true,
		TitleGenerationMaxDuration: 500 * time.Millisecond,
	})

	resultCh := make(chan struct {
		res *fantasy.AgentResult
		err error
	}, 1)
	go func() {
		res, err := a.Run(context.Background(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "first message",
			MaxOutputTokens: 1000,
		})
		resultCh <- struct {
			res *fantasy.AgentResult
			err error
		}{res, err}
	}()

	select {
	case r := <-resultCh:
		require.NoError(t, r.err, "Run must succeed despite the title provider hanging forever")
		require.NotNil(t, r.res, "expected a main-turn result even though title generation never completed")
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s — a hung title provider must not be able to block Run forever " +
			"(titleGenerationMaxDuration, shrunk to 500ms in this test, should have bounded the wait)")
	}

	// The session must still exist and, since the title provider never
	// answered, must have fallen back to the default name rather than being
	// left in some half-written state.
	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, DefaultSessionName, updated.Title,
		"expected the default-session-name fallback in generateTitle's deferred save to have landed")
}

// TestRun_TitleGeneratesNormally_StillAwaitedBeforeReturn is a sanity check
// that the fix did not turn title-wait into a no-op: with a fast, working
// title provider, Run() must still actually wait for generateTitle to save
// the title before returning (defer wg.Wait() must keep blocking on success,
// only the pathological/hung case should be cut short).
func TestRun_TitleGeneratesNormally_StillAwaitedBeforeReturn(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	srv := singleTurnSSEServer(nil)
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"A Real Title"},"finish_reason":null}]}`,
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
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	smallModel := Model{
		Model:      titleLM,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           smallModel,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})

	res, err := a.Run(context.Background(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "first message",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "A Real Title", updated.Title,
		"Run must still wait for a fast, successful title generation to land before returning")
}
