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
