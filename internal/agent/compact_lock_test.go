package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
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

// blockingSummarizeServer returns an httptest.Server that blocks until
// `started` is closed (signaling the test that the server is in the handler),
// then waits for `proceed` to close before responding. This allows tests to
// deterministically observe lock states during compaction without sleep races.
func blockingSummarizeServer(promptTokens, completionTokens int64, callCount *atomic.Int64, started, proceed chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			callCount.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Signal that we're in the handler (lock is held).
		if started != nil {
			close(started)
		}

		// Wait for the test to verify the lock state.
		if proceed != nil {
			<-proceed
		}

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

// TestP312_ManualCompactionHoldsOSLock_Deterministic uses a blocking LLM
// server to deterministically verify that manual compaction holds the OS
// lock during runSummarizeBody.
func TestP312_ManualCompactionHoldsOSLock_Deterministic(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	var calls atomic.Int64
	serverStarted := make(chan struct{})
	serverProceed := make(chan struct{})
	srv := blockingSummarizeServer(5, 2, &calls, serverStarted, serverProceed)
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
	env := testEnv(t)

	a := NewSessionAgent(SessionAgentOptions{
		DataDirectory:        dataDir,
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})

	sess, err := env.sessions.Create(t.Context(), "manual compact OS lock test")
	require.NoError(t, err)

	for i := 0; i < 4; i++ {
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "seed message"}},
		})
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	compactionDone := make(chan error, 1)
	go func() {
		compactionDone <- a.Summarize(ctx, sess.ID, fantasy.ProviderOptions{})
	}()

	// Unblock the handler no matter how this test exits. A require failure
	// below calls t.FailNow, which unwinds via runtime.Goexit and would skip
	// a bare close(serverProceed) — leaving the handler parked on the channel
	// and httptest's Close waiting ~10s on the outstanding request. The
	// resulting hang reads like a deadlock in the code under test rather than
	// the assertion failure it actually is.
	var proceedOnce sync.Once
	releaseServer := func() { proceedOnce.Do(func() { close(serverProceed) }) }
	defer releaseServer()

	// Wait for the server to start (lock should now be held).
	<-serverStarted

	// Verify the OS lock is held by trying to acquire it from "another process".
	externalLock, lockErr := session.TryAcquireSessionLock(dataDir, sess.ID)

	// The lock acquisition MUST fail with SessionLockBusyError.
	var busyErr *session.SessionLockBusyError
	require.ErrorAs(t, lockErr, &busyErr,
		"TryAcquireSessionLock must fail with SessionLockBusyError while manual compaction holds the OS lock")
	require.Nil(t, externalLock, "no lock should be returned when acquisition fails")

	// Release the server to allow compaction to complete.
	releaseServer()

	select {
	case err := <-compactionDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("compaction ended with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not finish")
	}

	// After compaction completes, the lock should be free. Verify we can acquire it now.
	externalLock2, lockErr2 := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, lockErr2, "after compaction, OS lock should be available")
	require.NotNil(t, externalLock2, "lock should be returned when acquisition succeeds")
	_ = externalLock2.Release()
}

// TestP312_InlineCompactionDoesNotDeadlock proves that inline compaction
// (runTurn's shouldSummarize branch calling runSummarizeBody directly) does
// NOT deadlock even though runSummarizeBody is now used by both manual and
// inline paths.
//
// The fix adds OS lock acquisition to runSummarize (manual path only), but
// inline compaction already holds the OS lock via the parent Run() call.
// No deadlock is possible because inline compaction never calls runSummarize.
func TestP312_InlineCompactionDoesNotDeadlock(t *testing.T) {
	// Deliberately NOT t.Parallel(), matching
	// TestRunTurn_ShouldSummarize_QueuedMessageContinuesWithoutLockDeadlock:
	// the call-count routing below is order-sensitive.
	env := testEnv(t)
	dataDir := t.TempDir()

	var mainCalls, summarizeCalls atomic.Int64
	requestStarted := make(chan struct{}, 1)
	var proceedOnce sync.Once
	proceed := make(chan struct{})
	mainSrv := thresholdCrossingSSEServer(&mainCalls, requestStarted, proceed)
	t.Cleanup(mainSrv.Close)
	summarizeSrv := summarizeSSEServer(5, 2, &summarizeCalls)
	t.Cleanup(summarizeSrv.Close)
	followUpSrv := summarizeSSEServer(5, 2, nil)
	t.Cleanup(followUpSrv.Close)

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
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(dispatchSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	// Separate, UNCOUNTED server for the small model / title generation.
	// This is not optional: needsTitle fires in the background on a
	// session's first turn, and pointing SmallModel at dispatchSrv lets
	// that request race into the cumulative-call-count routing above and
	// consume the "case 0" slot meant for the main turn. The threshold-
	// crossing usage then never reaches the main turn, shouldSummarize
	// never fires, and the test fails ~50% of the time with an empty
	// SummaryMessageID. TestRunTurn_ShouldSummarize_QueuedMessageContinuesWithoutLockDeadlock
	// carries the same isolation for the same reason.
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
		DataDirectory:        dataDir,
		LargeModel:           model,
		SmallModel:           smallModel,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: false, // Enable auto-summarize
	})

	sess, err := env.sessions.Create(t.Context(), "inline compaction deadlock test")
	require.NoError(t, err)

	// Queue a message after the stream starts so it's picked up by the
	// end-of-turn drain, not folded into turn 1's own prompt.
	go func() {
		select {
		case <-requestStarted:
		case <-time.After(5 * time.Second):
			return
		}
		sa := a.(*sessionAgent)
		sa.getMailbox(sess.ID).queue(SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "queued",
			MaxOutputTokens: 1000,
		})
		proceedOnce.Do(func() {
			close(proceed)
		})
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
		t.Fatal("Run did not return within 20s — possible deadlock on shouldSummarize")
	}

	// Must succeed without deadlock and without a lock error.
	require.NoError(t, runErr, "inline compaction must not deadlock or fail with lock error")
	var busyErr *session.SessionLockBusyError
	assert.False(t, errors.As(runErr, &busyErr), "must not be a SessionLockBusyError")

	// Verify that compaction actually occurred by checking the session state.
	// The compaction creates a summary message and sets SummaryMessageID.
	finalSess, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, finalSess.SummaryMessageID, "expected auto-summarize compaction to have created a summary")
	assert.Equal(t, int64(1), summarizeCalls.Load(), "expected exactly one summarize call")
}

// TestP312_ManualCompactionDrainsQueuedWork_WithDataDir is the regression
// test for the self-lock defect the first #312 draft shipped: runSummarize
// took the OS lock with a `defer lk.Release()`, which kept it held across
// the tail call to a.Run() for the queued follow-on turn. a.Run() then tried
// to acquire the very same lock and the process rejected itself with
// SessionLockBusyError naming its OWN pid.
//
// It is deliberately a fork of TestRunSummarize_QueuedMessage_RunsAsFollowOnTurn
// (summarize_queue_test.go) with ONE difference: DataDirectory is set. That
// difference is the entire point — the original leaves dataDir empty, so the
// `if a.dataDir != ""` guard around the lock never executes and the test is
// structurally blind to anything #312 does. The full package suite passed
// green under -race for 171s with the self-lock defect in place, because no
// test in it exercised this path with a real data directory.
func TestP312_ManualCompactionDrainsQueuedWork_WithDataDir(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	dataDir := t.TempDir()

	var summarizeCalls, followUpCalls atomic.Int64
	summarizeSrv := summarizeSSEServer(5, 2, &summarizeCalls)
	t.Cleanup(summarizeSrv.Close)
	followUpSrv := summarizeSSEServer(5, 2, &followUpCalls)
	t.Cleanup(followUpSrv.Close)

	// One largeModel serves both the summarize stream and the queued call's
	// own follow-on Run(), so route by call count rather than by model.
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
		DataDirectory:        dataDir,
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

	sess, err := env.sessions.Create(t.Context(), "manual compact drain with datadir")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed message"}},
	})
	require.NoError(t, err)

	queued := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued during manual compact",
		MaxOutputTokens: 1000,
	}
	sa.getMailbox(sess.ID).queue(queued)

	err = sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err,
		"Summarize must release the OS lock before draining the queued call into a fresh a.Run(); "+
			"holding it across that call makes the process reject itself with SessionLockBusyError")

	var busyErr *session.SessionLockBusyError
	require.False(t, errors.As(err, &busyErr),
		"a lock-busy error here means the process locked itself out, not that another process holds the session")

	require.Equal(t, int64(1), followUpCalls.Load(),
		"the queued message must actually reach the provider as its own follow-on turn")

	// The lock must not be leaked once Summarize has returned.
	lk, lkErr := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, lkErr, "the session lock must be free after Summarize returns")
	require.NotNil(t, lk)
	_ = lk.Release()
}

// TestP312_OSLockReleasedBeforeMailboxReportsIdle is the regression test for
// HIGH-1 found by an independent review of #312: runSummarize's first
// version called abandonOwnership (which flips the mailbox to mbIdle, what
// IsSessionBusy/IsBusy report to same-process callers) BEFORE releasing the
// OS session lock. Releasing the lock is unbounded disk I/O (Truncate/Seek/
// Sync/unlink sidecar/unlock/Close) with no timeout, so a same-process
// caller (a web UI request, `sessions inject`, restartOrphaned) could see
// "not busy" via IsSessionBusy, win submit(), and then hit
// TryAcquireSessionLock's SessionLockBusyError naming its OWN pid while this
// goroutine was still mid-Release. This is the exact class of bug mbReleasing
// (#296) exists to prevent, reopened by #312's own fix.
//
// Uses mailbox.testPreAbandonSeam — fired strictly after the OS lock is
// released and strictly before abandonOwnership runs — to deterministically
// park a check at that instant: the OS lock must already be free (another
// process could take it) while the mailbox must still report busy. No
// time.Sleep, no polling.
func TestP312_OSLockReleasedBeforeMailboxReportsIdle(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	dataDir := t.TempDir()

	var calls atomic.Int64
	srv := summarizeSSEServer(5, 2, &calls)
	t.Cleanup(srv.Close)
	lm := languageModelFromServer(t, srv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	a := NewSessionAgent(SessionAgentOptions{
		DataDirectory:        dataDir,
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

	sess, err := env.sessions.Create(t.Context(), "os-lock-before-idle test")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed message"}},
	})
	require.NoError(t, err)

	mb := sa.getMailbox(sess.ID)

	seamHit := make(chan struct{})
	mb.testPreAbandonSeam = func() {
		defer close(seamHit)

		// The OS lock must already be free: another process (simulated here
		// by a direct call, exactly like the other P312 tests) must succeed.
		externalLock, lockErr := session.TryAcquireSessionLock(dataDir, sess.ID)
		assert.NoError(t, lockErr, "OS lock must already be released by the time this seam fires")
		if externalLock != nil {
			_ = externalLock.Release()
		}

		// The mailbox must still report this session busy: abandonOwnership
		// has not run yet, so no same-process caller should be able to walk
		// in and become owner right now. The seam's own contract (mirroring
		// testLoopRearmSeam) is that it fires with mb.mu NOT held, so a plain
		// Lock here is safe.
		mb.mu.Lock()
		state := mb.state
		mb.mu.Unlock()
		assert.Equal(t, mbOwned, state,
			"mailbox must still report mbOwned — abandonOwnership must not have run yet")
	}
	t.Cleanup(func() { mb.testPreAbandonSeam = nil })

	err = sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{})
	require.NoError(t, err)

	select {
	case <-seamHit:
	default:
		t.Fatal("testPreAbandonSeam never fired — the seam wiring itself is broken, this test proves nothing")
	}
}

// oneChunkThenBlockServer sends a single text-delta chunk (enough to trigger
// runSummarizeBody's OnTextDelta -> notifyActivity(ctx) call), signals
// `started`, then blocks on `proceed` before completing the stream. Lets a
// test observe a real heartbeat tick landing WHILE the compaction is still
// in flight, with at least one activity recorded before the tick.
func oneChunkThenBlockServer(promptTokens, completionTokens int64, started, proceed chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`)
		if fl != nil {
			fl.Flush()
		}

		if started != nil {
			close(started)
		}
		if proceed != nil {
			<-proceed
		}

		fmt.Fprintf(w, "data: %s\n\n",
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
				promptTokens, completionTokens, promptTokens+completionTokens))
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// TestP312_ManualCompactionHeartbeatStaysAlive is the regression test for
// MEDIUM-1 found by an independent review of #312: runSummarize acquires its
// own OS session lock (separate from the one Run's turn loop holds), but
// never wired it into genCtx's activity-notify chain (task #222). Every
// notifyActivity(ctx) call runSummarizeBody's stream callbacks already made
// (#310) landed on a ctx with no activity-notify chain — a harmless no-op —
// so a long-running manual /compact's own lock never got RecordActivity
// calls and its mtime stayed frozen from acquisition to release. `sessions
// locks`/`watch`/`why` read that as heartbeat-stale for a session that is
// actually healthy and working — the same failure mode #310 fixed for
// silent compaction, reopened here for the manual path by #312's own lock.
//
// Real-time cost (~12s), matching the established convention in
// heartbeat_activity_test.go / internal/session/lock_test.go: the heartbeat
// goroutine is a real 10s ticker with no test seam, so there is no way to
// observe its effect deterministically without waiting for a real tick.
func TestP312_ManualCompactionHeartbeatStaysAlive(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	dataDir := t.TempDir()

	started := make(chan struct{})
	proceed := make(chan struct{})
	srv := oneChunkThenBlockServer(5, 2, started, proceed)
	t.Cleanup(srv.Close)
	lm := languageModelFromServer(t, srv)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	a := NewSessionAgent(SessionAgentOptions{
		DataDirectory:        dataDir,
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

	sess, err := env.sessions.Create(t.Context(), "manual compact heartbeat test")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed message"}},
	})
	require.NoError(t, err)

	// Same lock path Summarize's own runSummarize will acquire — grab it
	// once, up front, purely to learn its Path; release it immediately so
	// it doesn't block the real compaction below. Do NOT use this probe's
	// mtime as the "before" baseline: acquireSessionLockFile itself
	// Truncates+writes the PID into the file on every successful
	// acquisition, independent of the heartbeat/RecordActivity mechanism —
	// runSummarize's own TryAcquireSessionLock call below will stamp its
	// own PID and bump mtime again, AFTER this probe's snapshot would have
	// been taken. Snapshotting here would make the test pass even with the
	// activity-notify wiring completely removed (bumped mtime is from the
	// PID stamp, not the heartbeat) — caught only by revert-checking. The
	// real baseline is captured below, after `started` fires, i.e. after
	// runSummarize's own acquisition (and its PID stamp) has already
	// happened.
	probe, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err)
	lockPath := probe.Path
	require.NoError(t, probe.Release())

	compactionDone := make(chan error, 1)
	go func() {
		compactionDone <- sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction stream never started")
	}

	// Baseline taken AFTER runSummarize's own TryAcquireSessionLock (and its
	// PID-stamp mtime bump) has already happened — everything from here on
	// can only be the heartbeat, not the acquisition stamp.
	info, err := os.Stat(lockPath)
	require.NoError(t, err)
	before := info.ModTime()

	// The first chunk already landed (OnTextDelta fired, notifyActivity(ctx)
	// was called) before `started` closed. Wait past one real heartbeat tick
	// while the compaction is still parked on `proceed`, holding its lock.
	time.Sleep(12 * time.Second)

	info2, err := os.Stat(lockPath)
	require.NoError(t, err)
	assert.True(t, info2.ModTime().After(before),
		"manual /compact's own OS lock must record activity from its stream and stay heartbeat-fresh while genuinely working")

	close(proceed)

	select {
	case err := <-compactionDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not finish")
	}
}
