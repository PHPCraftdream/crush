package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failFastSSEServer answers every request with HTTP 400, driving
// agent.Stream (and therefore generateTitle's per-model attempt loop) down
// its err != nil branch immediately, without ever hanging a connection. 400
// is deliberately NOT one of fantasy's retryable statuses (408/409/429/5xx,
// see ProviderError.IsRetryable) so each attempt hits the server exactly
// once — a retryable status would hit the handler multiple times per
// attempt and complicate the single-fire handshake below for no benefit.
func failFastSSEServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadRequest)
	}))
}

// handshakeFailSSEServer signals requestStarted once it has received a
// request, then blocks until the test closes proceed, then answers with
// HTTP 400 (see failFastSSEServer's doc for why 400 and not 500: it is not
// retried). It lets a test pause generateTitle's LAST model attempt at the
// exact moment it is in flight — i.e. after the goroutine has committed to
// eventually reaching its deferred fallback, but before that fallback has
// run — so the test can deterministically inject state changes into that
// window without any sleep or real hang.
func handshakeFailSSEServer(requestStarted chan<- struct{}, proceed <-chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-proceed
		http.Error(w, "boom", http.StatusBadRequest)
	}))
}

// TestGenerateTitle_AbandonedFallbackDoesNotClobberLaterTitle is the
// regression test for #302: generateTitle runs detached from runTurn once
// its bounded join (P1-B, d76b5eba) gives up waiting on it, and its own
// deferred fallback (agent.go, generateTitle) used to stamp
// DefaultSessionName over the session's title UNCONDITIONALLY whenever this
// goroutine's own generation attempts failed or never got a chance to save
// one. That was safe only while runTurn waited unconditionally for this
// goroutine (a WaitGroup, pre-P1-B): the stamp always landed before the next
// turn could start, so nothing else could be racing it. Bounding the join
// (P1-B) broke that invariant — an abandoned goroutine can now finish
// minutes later, by which point a LATER turn may have already generated and
// saved a real title for the same session. Since runTurn's needsTitle check
// treats DefaultSessionName as "still needs a title", an unconditional
// unconditional stamp here does not just clobber once: every following turn
// spawns another title attempt, and if that attempt is also abandoned and
// also lands late, it clobbers again.
//
// The fix (2cfc71f9, hardened fail-closed by 8f2c8412) makes the deferred
// fallback re-read the session immediately before writing and only stamp
// DefaultSessionName when the title slot is still verifiably empty
// (sess.Title == "" || sess.Title == DefaultSessionName); if the re-read
// itself errors, it now also declines to write rather than falling through
// to the old unconditional rename.
//
// This test drives generateTitle directly (not through Run()) so the
// "later real title" write can be injected at the exact instant the
// abandoned goroutine is paused inside its last (large-model) attempt, via
// a request-started/proceed handshake — deterministic, no time.Sleep, no
// reliance on winning a race against a hung provider.
func TestGenerateTitle_AbandonedFallbackDoesNotClobberLaterTitle(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	// Small-model attempt fails immediately so generateTitle moves straight
	// to the large-model attempt without needing a second handshake.
	smallSrv := failFastSSEServer()
	t.Cleanup(smallSrv.Close)
	smallProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(smallSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	smallLM, err := smallProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	requestStarted := make(chan struct{})
	proceed := make(chan struct{})
	largeSrv := handshakeFailSSEServer(requestStarted, proceed)
	t.Cleanup(largeSrv.Close)
	largeProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(largeSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	largeLM, err := largeProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	cfg := turnConfig{
		smallModel: Model{
			Model:      smallLM,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
		largeModel: Model{
			Model:      largeLM,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	}

	a, ok := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	require.True(t, ok, "test relies on the concrete *sessionAgent to call the unexported generateTitle directly")

	done := make(chan struct{})
	go func() {
		defer close(done)
		// context.Background(), mirroring the detached genCtx a truly
		// abandoned title goroutine keeps running on in production (its
		// own titleCancel already fired, but generateTitle's deferred
		// fallback runs on context.WithoutCancel regardless).
		a.generateTitle(context.Background(), sess.ID, "first message", cfg)
	}()

	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("large-model title attempt never reached the server — handshake did not fire")
	}

	// While generateTitle is paused mid-attempt (i.e. exactly the window in
	// which production code would have already abandoned this goroutine and
	// moved on), simulate a LATER turn generating and saving a real title
	// for the same session.
	const laterRealTitle = "A Real Later Title"
	require.NoError(t, env.sessions.Rename(context.Background(), sess.ID, laterRealTitle))

	// Now let the abandoned goroutine's last attempt fail and its deferred
	// fallback run.
	close(proceed)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("generateTitle did not return after its last attempt was unblocked")
	}

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, laterRealTitle, updated.Title,
		"an abandoned title goroutine's deferred fallback must not overwrite a title a later turn already saved")
}

// TestGenerateTitle_FallbackStillFiresWhenSlotIsEmpty is a sanity check that
// the #302 fix did not turn the fallback into a no-op: when nothing else
// has set a title in the meantime, the deferred fallback must still stamp
// DefaultSessionName so a session is never left with no title at all.
func TestGenerateTitle_FallbackStillFiresWhenSlotIsEmpty(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	smallSrv := failFastSSEServer()
	t.Cleanup(smallSrv.Close)
	smallProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(smallSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	smallLM, err := smallProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	largeSrv := failFastSSEServer()
	t.Cleanup(largeSrv.Close)
	largeProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(largeSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	largeLM, err := largeProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	cfg := turnConfig{
		smallModel: Model{
			Model:      smallLM,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
		largeModel: Model{
			Model:      largeLM,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	}

	a, ok := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	require.True(t, ok)

	a.generateTitle(context.Background(), sess.ID, "first message", cfg)

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, DefaultSessionName, updated.Title,
		"expected the fallback to stamp the default name when nothing else claimed the title slot")
}
