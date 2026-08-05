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
// This test proves the fix (drainOrReleaseFinal now stores the reclaim
// cancel into mb.dispatcherCancel, and clears mb.current.cancel, both in
// the SAME critical section as the reclaim decision) by driving a real
// sa.Cancel(sess.ID) call and confirming turn 2 (the reclaimed call) never
// gets a chance to run to completion: its context is cancelled before its
// provider request would otherwise succeed.
//
// It uses TWO test-only seams, in sequence, to make the interleaving
// deterministic rather than racy:
//
//  1. mailbox.testDrainSeam fires inside drainOrReleaseFinal, strictly
//     before the legacy-queue reclaim runs. Released immediately — it only
//     signals "the reclaim is about to happen", so the test can then wait
//     for seam 2 instead of guessing with a sleep.
//  2. mailbox.testLoopRearmSeam fires in Run's turn loop (agent.go),
//     strictly AFTER the reclaim's critical section has already run to
//     completion (dispatcherCancel populated, current.cancel nil'd) and
//     mb.mu released, and strictly BEFORE that same loop iteration calls
//     beginGeneration(turnCancel) to re-arm current.cancel for turn 2. The
//     test parks the loop here, calls sa.Cancel(sess.ID) to completion
//     (guaranteed to observe the reclaim's already-fixed state, since the
//     loop cannot have re-armed yet), and only then releases the loop to
//     proceed with its own beginGeneration call.
//
// Earlier versions of this test used only seam 1 and assumed that was
// enough to "deterministically" land Cancel inside the reclaim window. That
// assumption was checked empirically (temporarily reverting just the
// mb.current.cancel = nil line in mailbox.go's reclaim branch, run
// repeatedly) and found false both before and after #284/#296: once seam 1
// releases and drainOrReleaseFinal's own mb.mu.Unlock() runs, there is a
// genuine, unguarded race for the NEXT mb.mu acquisition between a blocked
// Cancel() goroutine and the loop's own re-arm — regardless of whether the
// re-arm writes the SAME cancel value each iteration (true before #284) or
// a FRESH, distinct one every iteration (true after #284's turnCtx/
// turnCancel split). Either way the race existed; only the shape of what
// "losing" it looked like changed. Seam 2 removes the race by parking the
// loop before it can even attempt the re-arm.
//
// TestMailbox_DrainOrReleaseFinal_LegacyReclaimNeverReleasesLock
// (mailbox_lock_test.go) is, and remains, the authoritative regression
// guard for the underlying MEDIUM-1 defect: it asserts the exact same
// post-reclaim state directly, in the same goroutine, with no concurrency
// at all. This test is the additional real-Run()/real-Cancel() integration
// check — with seam 2 it is now deterministic too, not merely
// non-false-failing.
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

	// Arm the FIRST seam: fires inside runTurn's end-of-turn
	// drainOrReleaseMerged, strictly after mb.submitted has been observed
	// empty but strictly before the legacy-queue check/reclaim runs. mb.mu
	// is held for the whole pause — see mailbox.drainOrReleaseFinal's doc.
	// Released immediately (no gating on Cancel here — that used to be the
	// unreliable part, see testLoopRearmSeam below): this seam only exists
	// to let the test know the reclaim's critical section is ABOUT to run,
	// so it can wait for the SECOND seam next rather than racing a sleep
	// against it.
	//
	// Guarded with sync.Once and a no-op after the first pause: if the FIX
	// under test is broken (Cancel fails to actually stop turn 2), turn 2
	// runs to completion and reaches this SAME seam again at ITS OWN
	// end-of-turn drain. Without the guard that would double-close
	// seamEntered and panic, masking the real bug behind a test-harness
	// crash instead of a clean, informative assertion failure below.
	seamEntered := make(chan struct{})
	var seamOnce sync.Once
	mb.testDrainSeam = func() {
		seamOnce.Do(func() { close(seamEntered) })
	}

	// Arm the SECOND seam (#289 fix for this test's own flakiness):
	// mailbox.testLoopRearmSeam fires in Run's turn loop (agent.go) at the
	// top of EVERY iteration, including turn 1's — so the FIRST call is
	// turn 1's own loop entry (long before the reclaim this test cares
	// about even exists) and must pass straight through as a no-op. Only
	// the SECOND call is the one this test wants: it fires strictly AFTER
	// the reclaim's critical section above has already run and released
	// mb.mu (state == mbOwned, dispatcherCancel populated, current.cancel
	// nil — the fixed post-reclaim state), and strictly BEFORE that same
	// loop iteration calls beginGeneration(turnCancel) to re-arm
	// current.cancel for turn 2. Without this seam there is a genuine,
	// unguarded race for the next mb.mu acquisition between (a) a
	// concurrently-fired Cancel() goroutine and (b) the loop's own re-arm —
	// see this test's git history for the empirical failure rate that race
	// produced (13/20 runs failed to catch a reverted fix). Parking the
	// loop at the SECOND call, calling Cancel() to completion, and only
	// THEN releasing the loop removes the race entirely: Cancel() is
	// guaranteed to observe the reclaim's fixed state before
	// beginGeneration ever runs.
	loopRearmEntered := make(chan struct{})
	releaseLoopRearm := make(chan struct{})
	var loopRearmCalls atomic.Int64
	mb.testLoopRearmSeam = func() {
		if loopRearmCalls.Add(1) == 2 {
			close(loopRearmEntered)
			<-releaseLoopRearm
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

	// Wait for the loop to reach testLoopRearmSeam: by construction this can
	// only happen AFTER drainOrReleaseFinal's reclaim critical section has
	// already run to completion and released mb.mu (state == mbOwned,
	// dispatcherCancel == the freshly-passed reclaim cancel,
	// current.cancel == nil — see drainOrReleaseFinal's own doc for why),
	// and STRICTLY BEFORE this same loop iteration calls
	// beginGeneration(turnCancel) to re-arm current.cancel for turn 2. The
	// loop parks here, so there is no clock to race against: whatever
	// Cancel() observes below is guaranteed to be the reclaim's
	// post-critical-section state, not a coin flip against the re-arm.
	select {
	case <-loopRearmEntered:
	case err := <-runDone:
		t.Fatalf("Run returned before the loop-rearm seam was reached: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never reached testLoopRearmSeam after the reclaim")
	}

	// Fire Cancel from a separate goroutine and wait for it to run to
	// completion WHILE the loop is still parked in testLoopRearmSeam, i.e.
	// strictly before beginGeneration(turnCancel) can execute. This is what
	// makes the interleaving deterministic (#289): the old version of this
	// test only held mb.mu for the DRAIN seam, so once that seam released,
	// Cancel() and the loop's own re-arm raced for the next mb.mu
	// acquisition — verified empirically (temporarily reverting just the
	// mb.current.cancel = nil line in mailbox.go's reclaim branch) to catch
	// the regression only ~1/3-2/3 of the time across repeated runs, not
	// reliably. Parking the loop in a SECOND seam (testLoopRearmSeam,
	// mirroring testDrainSeam's existing idiom) before it ever calls
	// beginGeneration removes that race entirely: Cancel() is guaranteed to
	// complete before the loop's re-arm gets a chance to run at all.
	//
	// The mailbox-level TestMailbox_DrainOrReleaseFinal_LegacyReclaimNeverReleasesLock
	// (mailbox_lock_test.go) remains the authoritative, always-was-
	// deterministic regression guard for the underlying MEDIUM-1 defect —
	// it asserts the same post-reclaim state directly, in the same
	// goroutine, with no concurrency at all. This test is the additional
	// real-Run()-level, real-Cancel()-call integration check; with the
	// second seam it is now ALSO deterministic rather than merely
	// non-false-failing.
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		sa.Cancel(sess.ID)
	}()
	<-cancelDone

	// Only now let the loop proceed to beginGeneration(turnCancel) for
	// turn 2 — strictly after Cancel() has already returned.
	close(releaseLoopRearm)

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
