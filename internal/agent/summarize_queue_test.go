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
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers task #215: "queued message continues after a
// summarize/compact" has two call sites of runSummarizeCore
// (agent.go's runTurn shouldSummarize branch, and the top-level
// runSummarize/Summarize path) that handle its hasNext result completely
// differently, and neither had any test coverage before this file.
//
//  1. runTurn's nested shouldSummarize branch (mid-turn auto-summarize,
//     Run() still on the stack holding the OS lock) appends the drained
//     call back onto messageQueue instead of recursing into a.Run() — the
//     fix for the task #195 recursive-self-deadlock bug. TestRunTurn_* below
//     is the regression test for that fix on this specific path.
//  2. runSummarize (top-level /compact entry point, Run() NOT on the stack)
//     calls a.Run() directly on the drained call — safe there specifically
//     because no Run() already holds the lock.
//
// runSummarizeCore itself only pops the queue once, at its very tail, after
// the summarize stream is fully done — so a plain, synchronous
// messageQueue.Append call made any time before invoking runSummarizeCore
// (directly, or via runSummarize/runTurn) deterministically makes hasNext
// true. No concurrency machinery is needed for tests A/B below.

// summarizeSSEServer returns an httptest.Server that streams a single
// text-only completion whose final usage chunk reports promptTokens +
// completionTokens, in the same wire format as
// heartbeat_activity_test.go/run_queue_drain_test.go's fake servers. Used
// both for ordinary turns and for runSummarizeCore's own internal
// fantasy.NewAgent stream (the response shape it needs — OnTextDelta content
// plus a normal finish — is generic enough to serve both roles).
func summarizeSSEServer(promptTokens, completionTokens int64, callCount *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			callCount.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
				promptTokens, completionTokens, promptTokens+completionTokens),
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

// languageModelFromServer builds a fantasy.LanguageModel backed by srv, the
// same openaicompat wiring used throughout this package's tests.
func languageModelFromServer(t *testing.T, srv *httptest.Server) fantasy.LanguageModel {
	t.Helper()
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	return lm
}

// --- Test A: runSummarizeCore in isolation ---------------------------------

// newSummarizeCoreTestAgent builds a *sessionAgent wired for driving
// runSummarizeCore directly: a real session with one seeded message (so
// runSummarizeCore's len(msgs)==0 early-return at agent.go ~2350 doesn't
// short-circuit before reaching the queue-drain tail), plus a small
// ContextWindow so the model config is realistic (irrelevant to
// runSummarizeCore itself, which never checks ContextWindow — that's
// runTurn's shouldSummarize gate, exercised separately in test C).
func newSummarizeCoreTestAgent(t *testing.T) (*sessionAgent, session.Session) {
	t.Helper()
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

	sess, err := env.sessions.Create(t.Context(), "summarize core test")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed message"}},
	})
	require.NoError(t, err)

	return sa, sess
}

// TestRunSummarizeCore_QueuedMessage_ReturnsHasNext proves runSummarizeCore
// reports the message queued (via a plain, pre-call messageQueue.Append) as
// its `next`/`hasNext` return values, with no error.
func TestRunSummarizeCore_QueuedMessage_ReturnsHasNext(t *testing.T) {
	sa, sess := newSummarizeCoreTestAgent(t)

	queued := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued during summarize",
		MaxOutputTokens: 1000,
	}
	sa.messageQueue.Append(sess.ID, queued)

	next, hasNext, err := sa.runSummarizeCore(t.Context(), sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err)
	assert.True(t, hasNext, "expected hasNext=true when a message was queued before runSummarizeCore ran")
	assert.Equal(t, queued.Prompt, next.Prompt, "expected the drained call to match the queued call's Prompt")
	assert.Equal(t, sess.ID, next.SessionID)
}

// TestRunSummarizeCore_NoQueuedMessage_ReturnsNoNext is the negative
// counterpart: with nothing queued, hasNext must be false and next must be
// the zero value.
func TestRunSummarizeCore_NoQueuedMessage_ReturnsNoNext(t *testing.T) {
	sa, sess := newSummarizeCoreTestAgent(t)

	next, hasNext, err := sa.runSummarizeCore(t.Context(), sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err)
	assert.False(t, hasNext, "expected hasNext=false when nothing was queued")
	assert.Equal(t, SessionAgentCall{}, next, "expected the zero value when nothing was queued")
}

// --- Test B: runSummarize (top-level /compact entry point) -----------------

// TestRunSummarize_QueuedMessage_RunsAsFollowOnTurn is the regression proof
// for the "safe to call a.Run() here" comment at agent.go ~2287-2291:
// runSummarize's own a.Run(ctx, next) call for a message drained during a
// standalone (non-nested) summarize must actually execute as a real
// follow-on turn, producing a new assistant message — not merely return
// without error.
func TestRunSummarize_QueuedMessage_RunsAsFollowOnTurn(t *testing.T) {
	env := testEnv(t)

	var summarizeCalls, followUpCalls atomic.Int64
	summarizeSrv := summarizeSSEServer(5, 2, &summarizeCalls)
	t.Cleanup(summarizeSrv.Close)
	followUpSrv := summarizeSSEServer(5, 2, &followUpCalls)
	t.Cleanup(followUpSrv.Close)

	// runSummarizeCore builds its own fantasy.NewAgent against largeModel,
	// so the summarize stream and the queued call's own follow-on Run()
	// both go through largeModel — route by call count instead of using two
	// separate Model configs, since sessionAgent has exactly one
	// largeModel at a time.
	dispatchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if summarizeCalls.Load() == 0 {
			summarizeSrv.Config.Handler.ServeHTTP(w, r)
			return
		}
		followUpSrv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(dispatchSrv.Close)
	lm := languageModelFromServer(t, dispatchSrv)

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

	sess, err := env.sessions.Create(t.Context(), "run summarize test")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed message"}},
	})
	require.NoError(t, err)

	before, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	queued := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued during manual compact",
		MaxOutputTokens: 1000,
	}
	sa.messageQueue.Append(sess.ID, queued)

	err = sa.runSummarize(t.Context(), sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err, "runSummarize must not error when draining a queued message into a fresh a.Run() call")

	after, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Greater(t, len(after), len(before),
		"expected new messages (queued user prompt + its assistant response) to have been persisted by the follow-on Run()")

	foundQueuedUserMsg := false
	for _, m := range after {
		msg := m
		if msg.Role == message.User && msg.Content().Text == queued.Prompt {
			foundQueuedUserMsg = true
			break
		}
	}
	assert.True(t, foundQueuedUserMsg, "expected the queued prompt to show up as a real user message, proving it was actually processed as a follow-on turn")

	assert.Equal(t, int64(1), followUpCalls.Load(), "expected exactly one follow-on provider call for the queued message's own turn")
}

// --- Test C: runTurn's nested shouldSummarize branch ------------------------

// thresholdCrossingSSEServer streams a single text-only completion whose
// final usage chunk reports promptTokens+completionTokens high enough to
// cross runTurn's shouldSummarize StopWhen threshold for a small
// ContextWindow model (agent.go ~1922-1940): with ContextWindow=1000 and
// smallContextWindowRatio=0.2, threshold=200, so usage totalling >=800
// makes remaining=200<=200 fire.
//
// Like requestStartedSSEServer (run_lock_test.go's sibling file,
// run_queue_drain_test.go), it signals requestStarted after its first chunk
// and blocks on proceed before sending the finishing chunk — this lets a
// test append to messageQueue strictly AFTER PrepareStep's own TakeAll for
// this turn (which runs before any chunk is sent) has already drained the
// (at that point still empty) queue, and strictly BEFORE runSummarizeCore's
// tail-of-stream PopFront. Pre-queuing before Run() starts does NOT exercise
// the shouldSummarize drain path: PrepareStep's TakeAll would simply fold a
// pre-queued message into turn 1's own prompt instead.
func thresholdCrossingSSEServer(callCount *atomic.Int64, requestStarted chan<- struct{}, proceed <-chan struct{}) *httptest.Server {
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
		// total 800 >= 800 threshold trigger (ContextWindow=1000, ratio=0.2 -> threshold=200)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":600,"completion_tokens":200,"total_tokens":800}}`)
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// TestRunTurn_ShouldSummarize_QueuedMessageContinuesWithoutLockDeadlock is
// the critical regression test for task #215: it drives a REAL top-level
// a.Run() whose single turn's usage crosses the auto-summarize threshold,
// with a message queued WHILE that turn's stream is still in flight (after
// PrepareStep's own TakeAll has already run for turn 1 — see
// thresholdCrossingSSEServer's doc for why pre-queuing before Run() starts
// would not exercise this path), and a REAL DataDirectory (so Run() actually
// takes the OS SessionLock). Before task #195's fix, runTurn's
// shouldSummarize branch recursed into a.Run() to drain a queued message,
// which tried to re-acquire the OS lock its own still-executing Run() call
// already held, and failed with "already in use". The fix instead appends
// the drained call back onto messageQueue so the SAME Run() call's own turn
// loop picks it up via its ordinary end-of-turn PopFront — this test proves
// that happens: no lock error, the queued message is actually processed
// (not silently dropped), and the summary itself is also created.
func TestRunTurn_ShouldSummarize_QueuedMessageContinuesWithoutLockDeadlock(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	var mainCalls, summarizeCalls, followUpCalls atomic.Int64
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	mainSrv := thresholdCrossingSSEServer(&mainCalls, requestStarted, proceed)
	t.Cleanup(mainSrv.Close)
	summarizeSrv := summarizeSSEServer(5, 2, &summarizeCalls)
	t.Cleanup(summarizeSrv.Close)
	followUpSrv := summarizeSSEServer(5, 2, &followUpCalls)
	t.Cleanup(followUpSrv.Close)

	// The main turn, the summarize-agent's own internal stream, and the
	// queued message's own follow-on turn are all issued against the same
	// largeModel/provider, in that strict order — route by cumulative call
	// count so each phase gets a response shaped for its role (only the
	// first response needs the threshold-crossing usage; the rest just need
	// to be well-formed).
	dispatchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mainCalls.Load() {
		case 0:
			mainSrv.Config.Handler.ServeHTTP(w, r)
		default:
			if summarizeCalls.Load() == 0 {
				summarizeSrv.Config.Handler.ServeHTTP(w, r)
				return
			}
			followUpSrv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(dispatchSrv.Close)
	lm := languageModelFromServer(t, dispatchSrv)

	// Separate, uncounted server for the small model / title generation —
	// same rationale as run_queue_drain_test.go's newQueueDrainTestAgent:
	// needsTitle fires in the background on a session's first turn and must
	// not pollute the main-model call-count routing above.
	titleSrv := summarizeSSEServer(1, 1, nil)
	t.Cleanup(titleSrv.Close)
	titleLM := languageModelFromServer(t, titleSrv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 1000, DefaultMaxTokens: 1000},
	}
	smallModel := Model{
		Model:      titleLM,
		CatwalkCfg: catwalk.Model{ContextWindow: 1000, DefaultMaxTokens: 1000},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:    model,
		SmallModel:    smallModel,
		SystemPrompt:  "you are a probe",
		IsYolo:        true,
		Sessions:      env.sessions,
		Messages:      env.messages,
		Tools:         []fantasy.AgentTool{},
		DataDirectory: dataDir,
		// DisableAutoSummarize intentionally left false (default): the whole
		// point of this test is to exercise the real shouldSummarize gate.
	})
	sa := a.(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "shouldSummarize queue test")
	require.NoError(t, err)

	queued := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued during auto-summarize",
		MaxOutputTokens: 1000,
	}

	// Queue the second message from a goroutine that waits for
	// requestStarted (turn 1's stream has begun, i.e. PrepareStep for turn 1
	// has already run and drained the — at that point still empty — queue)
	// before appending, exactly like TestRun_QueueDrain_DoesNotDeadlockOnOwnSessionLock
	// in run_queue_drain_test.go. This guarantees the queued message is only
	// picked up by the END-of-turn drain inside runTurn's shouldSummarize
	// branch, not folded into turn 1's own prompt by PrepareStep.
	go func() {
		select {
		case <-requestStarted:
		case <-time.After(5 * time.Second):
			return
		}
		sa.messageQueue.Append(sess.ID, queued)
		close(proceed)
	}()

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

	var runErr error
	select {
	case r := <-resultCh:
		runErr = r.err
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return within 20s — possible deadlock on shouldSummarize's queued-message drain")
	}

	require.NoError(t, runErr, "Run must not fail with a lock error when draining a queued message after auto-summarize")
	var busyErr *session.SessionLockBusyError
	assert.False(t, errors.As(runErr, &busyErr), "must not be a SessionLockBusyError — that would mean the recursive-Run()-into-itself deadlock (task #195) regressed for this path")

	// The queued message must have actually been processed as a real
	// follow-on turn, not silently dropped.
	after, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	foundQueuedUserMsg := false
	for _, m := range after {
		msg := m
		if msg.Role == message.User && msg.Content().Text == queued.Prompt {
			foundQueuedUserMsg = true
			break
		}
	}
	assert.True(t, foundQueuedUserMsg, "expected the queued prompt to show up as a real user message after the auto-summarize turn completed")
	assert.Equal(t, int64(1), followUpCalls.Load(), "expected exactly one follow-on provider call for the queued message's own turn")

	// The compaction itself must also have happened: SummaryMessageID set,
	// proving both halves (compaction AND queued continuation) occurred.
	finalSess, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, finalSess.SummaryMessageID, "expected the auto-summarize compaction to have created and wired up a summary message")
	assert.Equal(t, int64(1), summarizeCalls.Load(), "expected exactly one summarize-agent provider call")

	// Lock must be fully released now that Run() has returned — proves the
	// turn loop (not a leaked recursive call) is what actually completed
	// the queued turn.
	lk, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err, "expected the session lock to be released after Run() finished draining the shouldSummarize queue")
	require.NoError(t, lk.Release())
}
