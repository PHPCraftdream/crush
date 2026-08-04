package agent

import (
	"context"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_CancelDuringLegacyReclaimWindow_ActuallyCancelsTurn2 is the
// end-to-end regression test for round 11 review, MEDIUM-1.
//
// Before the fix, drainOrReleaseMerged's legacy-queue reclaim (the same
// mechanism TestRun_LegacyQueueMessageDuringFinalDrain_DoesNotWedgeAcrossTurns
// exercises for HIGH-1 of round 10) called `mb.reclaimSameEra(epoch, nil)` —
// note the literal nil. reclaimSameEra set mb.state back to mbOwned and
// mb.dispatcherCancel to that nil, but never touched mb.current.cancel
// (already nilled out by the preceding drainOrRelease call). So immediately
// after a legacy-queue reclaim, BOTH mailbox cancel handles were nil, and
// stayed nil until turn 2 itself reached its own beginGeneration call in
// Run's loop.
//
// Any sa.Cancel(sessionID) landing in that window found nothing to cancel:
// genCancel := mb.current.cancel (nil); fallback mb.dispatcherCancel (also
// nil) -> `if genCancel != nil` skips the call entirely. A silent no-op —
// Ctrl-C/sessions kill/cost-cap landing in this narrow window would
// silently fail to interrupt the reclaimed turn.
//
// This test proves the fix (drainOrReleaseFinal now stores runCancel into
// mb.dispatcherCancel atomically, in the SAME critical section as the
// reclaim decision) by using mailbox.testDrainSeam to land a real
// sa.Cancel(sess.ID) call so that it can only complete once the reclaim's
// critical section (which sets mb.dispatcherCancel) has already run — since
// Cancel takes the same mb.mu — and confirms turn 2 (the reclaimed call)
// never gets a chance to run to completion: its context is cancelled before
// its provider request would otherwise succeed.
func TestRun_CancelDuringLegacyReclaimWindow_ActuallyCancelsTurn2(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "medium-1 reclaim cancel test")
	require.NoError(t, err)

	var calls atomic.Int64
	requestStarted := make(chan struct{}, 4)
	// proceed1 unblocks ONLY turn 1's request; turn 2 (if it ever reaches
	// the provider at all, which the fix must prevent) gets no proceed
	// signal and hangs forever on its own `<-proceed` inside the handler —
	// this is deliberate: it turns "turn 2 raced to complete before
	// runCancel's effect was observable" (a timing-dependent flake) into
	// "turn 2 either never reaches the provider (fix working) or reaches it
	// and hangs there (fix broken, detected via requestStarted firing a
	// second time, independent of completion timing)."
	proceed1 := make(chan struct{})
	var proceedCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)
		if fl != nil {
			fl.Flush()
		}
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		if proceedCalls.Add(1) == 1 {
			// Turn 1 only.
			<-proceed1
		} else {
			// Turn 2 (or later) must never get here if the fix works. Block
			// for a bounded duration (much longer than the test's own
			// detection window below, but NOT forever) rather than racing
			// completion timing — a hard select{} here would deadlock
			// httptest.Server.Close() waiting for this connection to
			// finish, hanging the whole test binary even on a correctly
			// PASSING run of the fix (Close() is called via t.Cleanup
			// regardless of outcome). The test detects "turn 2 reached the
			// provider at all" via requestStarted firing a second time,
			// well before this timeout or the client's own context expires.
			select {
			case <-r.Context().Done():
			case <-time.After(20 * time.Second):
			}
			return
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
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel:           Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SmallModel:           Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	sa := agentIface.(*sessionAgent)
	mb := sa.getMailbox(sess.ID)

	// Arm the seam: fires inside runTurn's end-of-turn drainOrReleaseMerged,
	// strictly after mb.submitted has been observed empty but strictly
	// before the legacy-queue check/reclaim runs. mb.mu is held for the
	// whole pause (both by the seam and, crucially, by sa.Cancel below,
	// which also needs mb.mu) — see mailbox.drainOrReleaseFinal's doc.
	//
	// The seam is guarded with sync.Once and, after the first pause, becomes
	// a no-op: if the FIX under test is broken (Cancel fails to actually
	// stop turn 2), turn 2 runs to completion and reaches this SAME seam
	// again at ITS OWN end-of-turn drain. Without the guard that would
	// double-close seamEntered and panic, masking the real bug behind a
	// test-harness crash instead of a clean, informative assertion failure
	// below.
	seamEntered := make(chan struct{})
	releaseSeam := make(chan struct{})
	var seamOnce sync.Once
	mb.testDrainSeam = func() {
		first := false
		seamOnce.Do(func() {
			first = true
			close(seamEntered)
		})
		if first {
			<-releaseSeam
		}
	}

	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "first message",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Turn 1 reaches the provider and blocks mid-stream (requestStarted
	// signaled, then blocked on proceed).
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1 never reached the provider")
	}

	// Queue the legacy message strictly during turn 1's in-flight stream —
	// same window TestRun_LegacyQueueMessageDuringFinalDrain_DoesNotWedgeAcrossTurns
	// uses for HIGH-1, so it can only be picked up by the end-of-turn drain's
	// legacy-queue fallback (the reclaim path this test targets), not folded
	// into turn 1's own PrepareStep.
	sa.messageQueue.Append(sess.ID, SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued via the legacy path, to be reclaimed as turn 2",
		MaxOutputTokens: 1000,
	})

	// Let turn 1 finish streaming — its end-of-turn drain runs next and
	// immediately blocks in the seam (mb.submitted is empty, so it reaches
	// the seam every time).
	close(proceed1)

	select {
	case <-seamEntered:
	case err := <-runDone:
		t.Fatalf("Run returned before the drain seam was reached: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1's end-of-turn drain never reached the test seam")
	}

	// Fire Cancel from a separate goroutine WHILE the seam still holds
	// mb.mu: Cancel blocks on mb.mu.Lock() until the seam (and the reclaim
	// critical section immediately following it) completes.
	//
	// NOTE — this is a best-effort integration check, NOT a deterministic
	// reproduction of the exact defect: once drainOrReleaseFinal's own
	// deferred mb.mu.Unlock() runs, there is a genuine, unguarded race for
	// the NEXT mb.mu acquisition between (a) this test's blocked Cancel
	// goroutine and (b) Run's OWN loop re-arming the mailbox via its
	// unconditional `beginGeneration(runCancel)` at the top of the for loop
	// (agent.go, before the next runTurn call) — which happens to write the
	// SAME runCancel into mb.current.cancel that a correct MEDIUM-1 fix
	// would also expose via the dispatcherCancel fallback, so the loop's
	// own re-arm winning the race incidentally masks a missing MEDIUM-1
	// fix too. Verified empirically (temporarily reverting just the
	// mb.current.cancel = nil line in mailbox.go's reclaim branch): this
	// test then catches the regression only ~1/3 of the time across
	// repeated runs, not reliably. The mailbox-level
	// TestMailbox_DrainOrReleaseFinal_LegacyReclaimNeverReleasesLock (see
	// mailbox_lock_test.go) asserts the same post-reclaim state directly,
	// in the same goroutine, with no race — THAT is the deterministic
	// regression guard for this defect. This test is kept as an additional
	// real-Run()-level sanity check: it never false-fails when the fix is
	// present, it just cannot be trusted alone to catch a reintroduction.
	// Making it genuinely deterministic would need a second test-only seam
	// at Run's loop-level re-arm point (mirroring mailbox.testDrainSeam) —
	// filed as backlog, not done here to keep this fix's scope bounded.
	var cancelWG sync.WaitGroup
	cancelWG.Add(1)
	cancelStarted := make(chan struct{})
	go func() {
		defer cancelWG.Done()
		close(cancelStarted)
		sa.Cancel(sess.ID)
	}()
	<-cancelStarted
	// Give the Cancel goroutine a moment to actually reach mb.mu.Lock() and
	// block there (best-effort — the subsequent release+wait below is what
	// actually guarantees correctness, this just makes the intended
	// interleaving likely rather than relying on it).
	time.Sleep(20 * time.Millisecond)

	close(releaseSeam)
	cancelWG.Wait()

	// Race Run() actually returning against turn 2 reaching the provider a
	// SECOND time (which, per the handler above, hangs forever rather than
	// completing — see its own doc). Exactly one of these must happen within
	// the bound:
	//   - Run returns first: the fix worked, runCancel's effect (via
	//     mailbox.dispatcherCancel) stopped turn 2 before it ever reached
	//     the provider (most likely: during its DB preamble).
	//   - requestStarted fires a second time first: the fix is broken —
	//     Cancel was a no-op, turn 2 reached the provider and is now stuck
	//     forever in the handler's `select {}` — a clear, non-flaky signal
	//     distinct from any completion-timing race.
	var runErr error
	runReturned := false
	turn2ReachedProvider := false
	deadline := time.After(10 * time.Second)
	for !runReturned {
		select {
		case runErr = <-runDone:
			runReturned = true
		case <-requestStarted:
			turn2ReachedProvider = true
			// Give Run a brief extra window to return anyway (e.g. it
			// reached the provider but genCtx cancellation still lands
			// before the stream is read) before declaring failure — but
			// don't wait as long as the full deadline, since a truly wedged
			// turn 2 will never return.
			select {
			case runErr = <-runDone:
				runReturned = true
			case <-time.After(2 * time.Second):
				runReturned = true // stop waiting; assertions below will fail informatively
			}
		case <-deadline:
			t.Fatal("neither Run() returned nor did turn 2 reach the provider within the deadline")
		}
	}

	require.False(t, turn2ReachedProvider, "turn 2 must never reach the provider at all — before the fix (both "+
		"mailbox cancel handles nil at reclaim time until turn 2's own beginGeneration), Cancel was a silent "+
		"no-op and turn 2 ran its DB preamble and reached the provider normally, exactly as if Cancel had never "+
		"been called")

	// THE core MEDIUM-1 assertion: Cancel, landing exactly in the legacy-
	// reclaim window (before turn 2's own beginGeneration), must have
	// actually cancelled the reclaimed turn's generation.
	require.Error(t, runErr, "Run must return an error: Cancel landing in the reclaim window must actually have "+
		"cancelled turn 2's generation — before the fix, Cancel was a silent no-op and turn 2 ran to completion "+
		"normally with runErr == nil")
	assert.ErrorIs(t, runErr, context.Canceled, "the error must specifically be the cancellation, not some "+
		"unrelated failure")
	assert.Equal(t, int64(1), calls.Load(), "turn 2 must never have reached the provider at all — its context "+
		"(derived from runCancel, cancelled via mailbox.dispatcherCancel) must already be done by the time its "+
		"DB preamble would otherwise let it start streaming; only turn 1's single call should be recorded")
	assert.False(t, sa.IsSessionBusy(sess.ID), "the session must not be left wedged busy after the cancelled call returns")
}
