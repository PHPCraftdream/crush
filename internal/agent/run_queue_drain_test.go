package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// singleTurnSSEServer returns an httptest.Server that streams one
// single-step, text-only completion per request (same wire format as
// checkpoint_stall_probe_test.go's probe server) — enough to drive a full
// Run() turn through fantasy's real Stream() plumbing without a network
// dependency. callCount, if non-nil, is incremented once per request.
func singleTurnSSEServer(callCount *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			callCount.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
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
}

// requestStartedSSEServer streams a single text delta, then signals
// requestStarted (so a test can act "mid-stream", after fantasy's
// PrepareStep/OnTextDelta callbacks have already fired for this turn) and
// blocks until the test closes proceed, then finishes the stream normally.
// This reproduces the window the cancel-drain and end-of-turn queue-drain
// branches in runTurn actually cover: a message queued WHILE this turn is
// still in flight, i.e. after PrepareStep already drained messageQueue for
// this turn's own prompt — pre-queuing before Run() starts does not
// exercise that path, since PrepareStep's TakeAll would just fold the
// pre-queued message into the same turn's prompt instead.
func requestStartedSSEServer(callCount *atomic.Int64, requestStarted chan<- struct{}, proceed <-chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			callCount.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)
		if fl != nil {
			fl.Flush()
		}
		if requestStarted != nil {
			select {
			case requestStarted <- struct{}{}:
			default:
			}
		}
		if proceed != nil {
			<-proceed
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

func newQueueDrainTestAgent(t *testing.T, env fakeEnv, dataDir string, callCount *atomic.Int64) (SessionAgent, *httptest.Server) {
	t.Helper()
	srv := singleTurnSSEServer(callCount)
	t.Cleanup(srv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	// Separate, uncounted server for the small model: generateTitle always
	// fires in the background on a session's first turn (needsTitle is true
	// whenever len(msgs)==0) — that traffic must not pollute callCount,
	// which counts only the main-turn requests callers of this helper care
	// about.
	titleSrv := singleTurnSSEServer(nil)
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
		DataDirectory:        dataDir,
	})
	return a, srv
}

// TestRun_QueueDrain_DoesNotDeadlockOnOwnSessionLock is the regression test
// for the recursive-Run()-self-deadlock bug: before this fix, once a turn
// finished and found a message waiting in messageQueue (queued WHILE the
// turn was still in flight, e.g. via the "interrupt and send" flow, or a
// fast follow-up prompt), Run() called itself — a.Run(ctx,
// firstQueuedMessage) — from deep inside its own still-executing stack
// frame, before the `defer ipcLock.Release()` from the ORIGINAL call had
// run. Since session.TryAcquireSessionLock takes a real OS-level file lock
// (not a reentrant in-process mutex), that recursive call tried to acquire
// a lock its own parent frame was still holding and was rejected with
// "already in use" — so the queued message was silently dropped/failed
// instead of running as a second turn.
//
// This test drives Run() with DataDirectory set (so the real
// session.TryAcquireSessionLock path is exercised, not skipped) and queues
// the second message from WITHIN the SSE server handler itself — between
// the first and second chunk of the first turn's response — so it lands in
// messageQueue strictly AFTER PrepareStep's own TakeAll for turn 1 (which
// runs before any chunk is sent) and strictly BEFORE runTurn's end-of-turn
// PopFront (which runs after the whole stream completes). Queuing before
// Run() is even called would NOT exercise this: PrepareStep's TakeAll
// would just fold a pre-queued message into turn 1's own prompt instead of
// leaving it for the end-of-turn drain (agent.go's old line ~1839-1844, now
// runTurn's final PopFront). If the old recursive shape were still in
// place, the second (drained) turn would fail with a SessionLockBusyError;
// with the turn-loop fix, both turns run and Run() returns the second
// turn's result with no error.
func TestRun_QueueDrain_DoesNotDeadlockOnOwnSessionLock(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()
	var calls atomic.Int64

	sess, err := env.sessions.Create(t.Context(), "queue drain test")
	require.NoError(t, err)

	var a SessionAgent
	var sa *sessionAgent
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)

	// Queue the second message from a goroutine that waits for
	// requestStarted (turn 1's stream has begun, i.e. PrepareStep for turn
	// 1 has already run and drained the — at that point still empty —
	// queue) before appending. This guarantees ordering relative to
	// PrepareStep's TakeAll without needing to touch fantasy internals.
	go func() {
		select {
		case <-requestStarted:
		case <-time.After(5 * time.Second):
			return
		}
		sa.messageQueue.Append(sess.ID, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "second message",
			MaxOutputTokens: 1000,
		})
		close(proceed)
	}()

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	// Separate, uncounted server for the small model: turn 1 always fires
	// generateTitle in the background (needsTitle is true whenever
	// len(msgs)==0, i.e. on every session's first turn, regardless of the
	// title passed to sessions.Create) — that traffic must not pollute
	// `calls`, which is meant to count only the main-turn requests this
	// test cares about.
	titleSrv := singleTurnSSEServer(nil)
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
	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           smallModel,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	a = agentIface
	sa = a.(*sessionAgent)

	resultCh := make(chan struct {
		res *fantasy.AgentResult
		err error
	}, 1)
	go func() {
		res, err := a.Run(t.Context(), SessionAgentCall{
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
		require.NoError(t, r.err,
			"Run must not fail with a session-lock error when draining a queued message — "+
				"a SessionLockBusyError here means the old recursive-Run() self-deadlock regressed")
		var busyErr *session.SessionLockBusyError
		assert.False(t, errors.As(r.err, &busyErr), "must not be a SessionLockBusyError")
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s — possible deadlock draining the queued message")
	}

	// Both turns must have actually reached the provider: one for "first
	// message", one for the drained "second message". If the recursive call
	// had been rejected by the lock, only one request would have landed.
	assert.Equal(t, int64(2), calls.Load(),
		"expected two provider calls (initial turn + drained queued turn)")

	// The queue must be empty afterward — the drained message must not be
	// left stuck (which is what silently happened before this fix, since
	// the rejected recursive Run() returned an error without re-queuing).
	assert.Equal(t, 0, sa.QueuedPrompts(sess.ID))

	// Session lock must be fully released now that Run() has returned:
	// prove it by acquiring it fresh from outside.
	lk, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err, "expected the session lock to be released after Run() finished draining the queue")
	require.NoError(t, lk.Release())
}

// blockingReserveSessionService's Get blocks on a signal from the test
// after announcing it was entered, standing in for the slow DB preamble
// that separates the busy-check from the busy-registration in the
// pre-fix code. Used to pin down the exact instant a Run() call has
// claimed the busy reservation but not yet finished its turn, so a second
// concurrent Run() call for the same session can be driven into that exact
// window deterministically instead of racing goroutine scheduling.
type blockingReserveSessionService struct {
	session.Service
	entered  chan struct{}
	unblock  chan struct{}
	enterOne sync.Once
}

// Get only blocks on its FIRST invocation (the DB preamble at the very top
// of runTurn). Later calls in the same turn — e.g. OnStepFinish re-fetches
// the session — must pass through untouched, or the whole turn would
// deadlock on a channel that's already closed/drained.
func (b *blockingReserveSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	b.enterOne.Do(func() {
		close(b.entered)
		<-b.unblock
	})
	return b.Service.Get(ctx, id)
}

// TestRun_ConcurrentSameSession_SecondCallQueuesInsteadOfRacingReservation
// is the regression test for the second bug described in the task: the
// busy-check (IsSessionBusy) and the busy-registration
// (activeRequests.Set) used to happen as two separate steps, separated by
// the DB preamble (sessions.Get / getSessionMessages / createUserMessage).
// Two concurrent Run() calls for the SAME sessionID could both read "not
// busy" inside that window and both proceed toward the OS lock, instead of
// the second one being cleanly queued by the in-process check as intended.
//
// This test pins the first Run() call inside its DB preamble (via
// blockingReserveSessionService) — i.e. demonstrably PAST
// tryReserveSession, holding the reservation — and then starts a second
// Run() call for the same session while the first is still blocked there.
// With the atomic reserve-or-queue fix, the second call must be queued
// immediately and deterministically (observable via QueuedPrompts) without
// ever needing to reach — let alone race — the OS lock or the provider.
func TestRun_ConcurrentSameSession_SecondCallQueuesInsteadOfRacingReservation(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()
	var calls atomic.Int64
	a, _ := newQueueDrainTestAgent(t, env, dataDir, &calls)
	sa := a.(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "concurrent same session test")
	require.NoError(t, err)

	blocking := &blockingReserveSessionService{Service: sa.sessions, entered: make(chan struct{}), unblock: make(chan struct{})}
	sa.sessions = blocking

	firstDone := make(chan error, 1)
	go func() {
		_, err := a.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "first message",
			MaxOutputTokens: 1000,
		})
		firstDone <- err
	}()

	select {
	case <-blocking.entered:
		// Good: the first Run() call is now demonstrably past
		// tryReserveSession (blockingReserveSessionService.Get is the very
		// next call after the reservation + OS lock in runTurn) and holding
		// the busy reservation.
	case err := <-firstDone:
		t.Fatalf("first Run() returned before reaching sessions.Get (err=%v) — reservation never held long enough to test the race", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first Run() call to reach sessions.Get")
	}

	// Second call for the SAME session, while the first demonstrably still
	// holds the reservation. Must be queued immediately — Run()'s busy path
	// returns (nil, nil) without blocking.
	secondResultCh := make(chan struct {
		res *fantasy.AgentResult
		err error
	}, 1)
	go func() {
		res, err := a.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "second message",
			MaxOutputTokens: 1000,
		})
		secondResultCh <- struct {
			res *fantasy.AgentResult
			err error
		}{res, err}
	}()

	select {
	case r := <-secondResultCh:
		assert.NoError(t, r.err)
		assert.Nil(t, r.res, "the queued (losing) call must return (nil, nil) immediately, not a result")
	case <-time.After(5 * time.Second):
		t.Fatal("second Run() call for the same session did not return promptly — it must be queued immediately by the atomic reserve, not block")
	}

	assert.Equal(t, 1, sa.QueuedPrompts(sess.ID),
		"the second call must have been queued while the first holds the reservation")
	assert.Equal(t, int64(0), calls.Load(),
		"the second call must never have reached the provider while the first holds the reservation")

	// Let the first call's preamble proceed and the turn complete. The
	// queued second message was appended before the first turn's own
	// PrepareStep has run (PrepareStep is gated behind the same blocked
	// sessions.Get call), so PrepareStep's messageQueue.TakeAll folds it
	// into turn 1's own prompt (one provider call covering both messages)
	// rather than running it as a separate turn 2 — the queue-drain-as-a-
	// separate-turn path (runTurn's end-of-turn PopFront /
	// TestRun_QueueDrain_DoesNotDeadlockOnOwnSessionLock) only kicks in for
	// a message queued AFTER PrepareStep has already run for that turn.
	// Either way, the queue must end up empty and the call must succeed —
	// which is what actually matters for this test (the atomic reserve
	// preventing the race), the exact mechanics of when the queued message
	// is folded in are secondary.
	close(blocking.unblock)
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("first Run() call did not complete after unblocking")
	}
	assert.Equal(t, int64(1), calls.Load(), "expected the queued second message to be folded into turn 1's own PrepareStep, not run as a separate provider call")
	assert.Equal(t, 0, sa.QueuedPrompts(sess.ID))
}
