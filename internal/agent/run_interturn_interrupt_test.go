package agent

import (
	"context"
	"fmt"
	"io"
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

// TestRun_InterruptDuringInterTurnWindow_ReplacementRunsExactlyOnce is the
// end-to-end regression test for #307 (P1-2 follow-up): the window BETWEEN
// two turns, after Run's loop has cancelled the just-finished turn's
// turnCancel but strictly before it calls beginGeneration for the next one.
//
// Mechanism (see mailbox.interruptAndReplace's and reclaimReplacementOrKeep's
// own docs for the full account): drainOrReleaseFinal's every "keep
// ownership" branch (submitted/replacement/legacy-reclaim) leaves
// mb.current.cancel == nil — that is the exact postcondition
// mailbox_invariant_test.go's table checks. The loop only repopulates it via
// beginGeneration at the TOP of its next iteration. An InterruptAndReplace
// landing in that gap used to fall back to mb.dispatcherCancel, which is
// runCancel — the parent of every future turnCtx, never meant to be an
// interrupt target. Firing it there cancelled the whole dispatcher: the next
// turnCtx was born already-cancelled, its preamble failed immediately with
// context.Canceled, #284's preamble-recovery drained the mailbox once more
// (finding nothing, since the fix's own replacement had already been
// consumed or the recovery ran on a dead runCtx with nothing left to run it
// on), and Run returned an error — the replacement accepted, reported
// "queued", never actually executed.
//
// This test drives that exact interleaving deterministically using
// testLoopRearmSeam (#289's fix for exactly this kind of race — see that
// seam's own doc in mailbox.go) instead of a sleep-based race:
//
//  1. Turn 1 runs "first message" and blocks mid-stream.
//  2. While turn 1 is blocked, a STALE call is queued into the legacy
//     messageQueue — this is what drainOrReleaseMerged will decide to run
//     as turn 2 once turn 1 finishes (mb.submitted/mb.replacement are both
//     empty at that point, so it falls through to the legacy reclaim).
//  3. Turn 1 is allowed to finish. Its own end-of-turn drain reclaims the
//     stale call and returns hasNext=true; the loop goes to its next
//     iteration with call == the STALE call.
//  4. testLoopRearmSeam fires for the SECOND time (the first firing is turn
//     1's own loop entry, long before any of this) — by construction this
//     is strictly after the reclaim's critical section has already run
//     (mb.current.cancel nil, dispatcherCancel == runCancel) and strictly
//     before this same iteration's beginGeneration call. The loop parks
//     here.
//  5. While parked, the test calls sa.InterruptAndReplace with a REPLACEMENT
//     call and waits for it to return (hadOwner must be true).
//  6. The loop is released. It must run the REPLACEMENT, not the stale
//     call, as turn 2 — and that turn must actually reach the provider.
//
// Before the fix: step 5's InterruptAndReplace fired dispatcherCancel
// (runCancel), so step 6 aborted before the provider was ever reached again;
// Run returned a context.Canceled error and the replacement was never run at
// all — same shape as P0-A/P0-B, just via a different window.
//
// Closing-review update: an earlier draft of reclaimReplacementOrKeep
// discarded the pre-empted stale call instead of requeuing it (the #283/
// P0-2 class of bug — "interrupt deletes the very message it's supposed to
// queue behind"). This test's final assertions reflect the CORRECTED
// contract: the stale message is not lost, it is requeued and runs as turn
// 3 after the replacement. TestRun_InterruptDuringInterTurnWindow_MultiElementQueue_NoMessageLost
// below is the more direct regression test for that specific defect (it
// exercises the A/B/C-already-queued scenario the discard bug actually
// depended on to be observable); this test still stands on its own as the
// single-stale-message, real-Run()-level proof that the replacement itself
// pre-empts and reaches the provider.
func TestRun_InterruptDuringInterTurnWindow_ReplacementRunsExactlyOnce(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "307 inter-turn interrupt test")
	require.NoError(t, err)

	var calls atomic.Int64
	var promptsMu sync.Mutex
	var promptsSeen []string
	requestStarted := make(chan struct{}, 4)
	proceed1 := make(chan struct{})
	var proceedCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		promptsMu.Lock()
		promptsSeen = append(promptsSeen, string(body))
		promptsMu.Unlock()

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
			// Turn 1 only: block mid-stream until the test says go.
			<-proceed1
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

	// Arm testLoopRearmSeam: fires at the TOP of every loop iteration. The
	// first firing is turn 1's own entry (long before the window under
	// test); the second is the one this test cares about — see this
	// function's own doc for the exact ordering guarantee.
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

	// Turn 1 reaches the provider and blocks mid-stream.
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1 never reached the provider")
	}

	// Queue a STALE legacy message while turn 1 is still in flight — this is
	// what the end-of-turn drain will reclaim and hand to the loop as `call`
	// for its next iteration, strictly BEFORE the interrupt below lands.
	sa.getMailbox(sess.ID).queue(SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "STALE queued message, must never reach the provider",
		MaxOutputTokens: 1000,
	})

	// Let turn 1 finish. Its own end-of-turn drain reclaims the stale
	// message (mb.submitted/mb.replacement empty at that instant) and the
	// loop proceeds into its next iteration with call == the stale message.
	close(proceed1)

	// Wait for the loop to park at its SECOND testLoopRearmSeam call — by
	// construction this is strictly after the reclaim's critical section
	// has already released mb.mu, and strictly before this iteration's own
	// beginGeneration call.
	select {
	case <-loopRearmEntered:
	case err := <-runDone:
		t.Fatalf("Run returned before the loop-rearm seam was reached: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never reached testLoopRearmSeam for the second time")
	}

	// Fire InterruptAndReplace from a separate goroutine while the loop is
	// parked, and wait for it to return. This is what makes the
	// interleaving deterministic instead of racing the loop's own re-arm
	// for the next mb.mu acquisition (#289's fix, reused here for the same
	// reason).
	const replacementPrompt = "REPLACEMENT message from InterruptAndReplace"
	interruptDone := make(chan bool, 1)
	go func() {
		hadOwner := sa.InterruptAndReplace(sess.ID, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          replacementPrompt,
			MaxOutputTokens: 1000,
		})
		interruptDone <- hadOwner
	}()

	var hadOwner bool
	select {
	case hadOwner = <-interruptDone:
	case <-time.After(10 * time.Second):
		t.Fatal("InterruptAndReplace never returned while the loop was parked at testLoopRearmSeam")
	}
	require.True(t, hadOwner, "InterruptAndReplace must report an owner existed — the mailbox is still mbOwned "+
		"between turns, only current.cancel is transiently nil")

	// Only now let the loop proceed to beginGeneration for turn 2.
	close(releaseLoopRearm)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "Run must complete successfully: the interrupt landing in the inter-turn "+
			"window must NOT poison runCtx (dispatcherCancel) and kill the whole dispatcher — before the fix, "+
			"interruptAndReplace fell back to dispatcherCancel here, cancelling every future turn's context")
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned — the dispatcher is wedged")
	}

	// CORRECTED contract (closing review): the stale message is requeued,
	// not discarded, so it still runs — as turn 3, after the replacement.
	// Exactly three provider calls: turn 1, the replacement (pre-empting,
	// as turn 2), then the previously-stale message (turn 3, since nothing
	// else was queued behind it).
	require.Equal(t, int64(3), calls.Load(), "exactly three provider calls must have been made: turn 1, then the "+
		"replacement (pre-empting as turn 2), then the previously-stale message requeued as turn 3 — it must be "+
		"requeued, not discarded (closing review: an earlier draft of reclaimReplacementOrKeep silently dropped it, "+
		"the #283/P0-2 class of bug)")

	promptsMu.Lock()
	defer promptsMu.Unlock()
	require.Len(t, promptsSeen, 3)
	assert.Contains(t, promptsSeen[0], "first message", "turn 1 must have sent the original prompt")
	assert.Contains(t, promptsSeen[1], replacementPrompt, "turn 2 must have sent the REPLACEMENT prompt — it must "+
		"pre-empt the stale message, not merely run after it")
	assert.Contains(t, promptsSeen[2], "STALE queued message", "the previously-stale message must still run, as "+
		"turn 3 — requeued behind the replacement, not lost")

	assert.False(t, sa.IsSessionBusy(sess.ID), "the session must not be left wedged busy after the replacement turn completes")
}

// TestRun_InterruptDuringInterTurnWindow_MultiElementQueue_NoMessageLost is
// the direct end-to-end regression test for the closing-review defect: an
// earlier draft of reclaimReplacementOrKeep discarded the `call` it
// pre-empted instead of requeuing it — invisible with only one message
// queued (there was nothing left to distinguish "discarded" from "consumed
// as the pre-empted call"), but with MORE than one message already queued
// it silently destroyed exactly the one message a drain had already popped
// out of mb.submitted, while its siblings survived untouched — the choice
// of victim decided purely by scheduling luck. This is the #283/P0-2 class
// of bug: "the interrupt deletes the very message it's supposed to queue
// behind."
//
// Scenario, exactly as specified in the closing review:
//
//  1. Turn 1 runs and blocks mid-stream.
//  2. While turn 1 is blocked, THREE more Run() calls are made for the same
//     session — A, B, then C — each queues into mb.submitted via
//     mailbox.submit (Run's own busy-queue path), in that order.
//  3. Turn 1 finishes. Its own end-of-turn drain (drainOrReleaseFinal)
//     pops A out of mb.submitted (the FIFO head) as `call` for the loop's
//     next iteration; B and C remain in mb.submitted.
//  4. testLoopRearmSeam parks the loop strictly before that iteration's
//     beginGeneration — i.e. strictly before `call` (A) is committed to.
//  5. While parked, InterruptAndReplace fires with D.
//  6. The loop is released.
//
// Expected (the FIXED contract): D runs next (turn 2) — the interrupt's
// explicit request to pre-empt is honored. A is NOT discarded: it is pushed
// back onto the front of mb.submitted, so it runs as turn 3, followed by B
// (turn 4) and C (turn 5) — original FIFO order preserved among the
// deferred messages, only D allowed to jump the queue.
func TestRun_InterruptDuringInterTurnWindow_MultiElementQueue_NoMessageLost(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "307 multi-element queue interrupt test")
	require.NoError(t, err)

	var promptsMu sync.Mutex
	var promptsSeen []string
	requestStarted := make(chan struct{}, 8)
	proceed1 := make(chan struct{})
	var proceedCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		promptsMu.Lock()
		promptsSeen = append(promptsSeen, string(body))
		promptsMu.Unlock()

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
			// Turn 1 only: block mid-stream until the test says go.
			<-proceed1
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

	// Arm testLoopRearmSeam: fires at the TOP of every loop iteration. The
	// first firing is turn 1's own entry; the second is the one this test
	// cares about — strictly after the drain that popped A out of
	// mb.submitted, strictly before beginGeneration commits to running A.
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
			Prompt:          "turn 1 - the initial message",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Turn 1 reaches the provider and blocks mid-stream.
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1 never reached the provider")
	}

	// Queue A, B, C — in that order — via three more Run() calls for the
	// SAME session while turn 1 is still in flight. Each one queues into
	// mb.submitted via mailbox.submit (Run's busy-queue path) and returns
	// immediately with (nil, nil) since it never becomes owner.
	for _, prompt := range []string{"A - must run third", "B - must run fourth", "C - must run fifth"} {
		res, queueErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          prompt,
			MaxOutputTokens: 1000,
		})
		require.NoError(t, queueErr, "queuing behind a busy session must not itself error")
		require.Nil(t, res, "a queued call must return (nil, nil) — it does not run inline")
	}
	require.Equal(t, []string{"A - must run third", "B - must run fourth", "C - must run fifth"},
		promptsOf(mb.submitted), "precondition: A, B, C must all be sitting in mb.submitted, in FIFO order, "+
			"before turn 1 is allowed to finish")

	// Let turn 1 finish. Its own end-of-turn drain pops A out of
	// mb.submitted (FIFO head) as `call` for the loop's next iteration; B
	// and C remain queued.
	close(proceed1)

	// Wait for the loop to park at its SECOND testLoopRearmSeam call —
	// strictly after the drain above has already popped A and released
	// mb.mu, strictly before this iteration's own beginGeneration call
	// would commit to running A.
	select {
	case <-loopRearmEntered:
	case err := <-runDone:
		t.Fatalf("Run returned before the loop-rearm seam was reached: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never reached testLoopRearmSeam for the second time")
	}

	// Fire InterruptAndReplace with D from a separate goroutine while the
	// loop is parked, and wait for it to return.
	const replacementPrompt = "D - the interrupt's replacement, must run second"
	interruptDone := make(chan bool, 1)
	go func() {
		hadOwner := sa.InterruptAndReplace(sess.ID, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          replacementPrompt,
			MaxOutputTokens: 1000,
		})
		interruptDone <- hadOwner
	}()

	var hadOwner bool
	select {
	case hadOwner = <-interruptDone:
	case <-time.After(10 * time.Second):
		t.Fatal("InterruptAndReplace never returned while the loop was parked at testLoopRearmSeam")
	}
	require.True(t, hadOwner, "InterruptAndReplace must report an owner existed")

	// Only now let the loop proceed to beginGeneration for turn 2 (D).
	close(releaseLoopRearm)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "Run must complete successfully")
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned — the dispatcher is wedged")
	}

	promptsMu.Lock()
	defer promptsMu.Unlock()
	require.Len(t, promptsSeen, 5, "exactly five provider calls: turn 1, D (pre-empting as turn 2), then A, B, C "+
		"in their original order (turns 3-5) — none of A, B, or C may be lost, and none may be reordered relative "+
		"to each other; only D is allowed to jump ahead of them")
	assert.Contains(t, promptsSeen[0], "turn 1 - the initial message", "turn 1 must have sent the original prompt")
	assert.Contains(t, promptsSeen[1], replacementPrompt, "D must run SECOND — it pre-empts A, B, and C, which is "+
		"the whole point of interrupt-and-replace")
	assert.Contains(t, promptsSeen[2], "A - must run third", "A must survive and run third — it must NOT be "+
		"discarded just because a drain had already popped it out of mb.submitted before the interrupt landed "+
		"(closing review: the #283/P0-2 class of bug this test exists to catch)")
	assert.Contains(t, promptsSeen[3], "B - must run fourth", "B must survive and run fourth, preserving its "+
		"original position relative to A and C")
	assert.Contains(t, promptsSeen[4], "C - must run fifth", "C must survive and run fifth, preserving its "+
		"original position relative to A and B")

	assert.False(t, sa.IsSessionBusy(sess.ID), "the session must not be left wedged busy after all five turns complete")
}

// promptsOf extracts the Prompt field from a slice of SessionAgentCall, for
// asserting mb.submitted's contents/order directly.
func promptsOf(calls []SessionAgentCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Prompt
	}
	return out
}

// TestRun_InterruptBeforeFirstGeneration_ReplacementRunsExactlyOnce verifies
// that the SAME fix also covers the window BEFORE the very first turn's own
// beginGeneration call — i.e. the window MEDIUM-1 (round 9 review, the
// predecessor of #307) originally targeted, before #284's turnCtx/turnCancel
// split opened up the structurally identical inter-turn window this whole
// file is otherwise about.
//
// This is not assumed to be "the same bug, don't need a separate test" —
// verified directly, because the two windows differ in one way that could
// plausibly have mattered: mb.state transitions to mbOwned INSIDE submit()
// (mailbox.go), called from tryReserveSession, BEFORE Run() ever reaches its
// turn loop — as opposed to the inter-turn window, where state was already
// mbOwned from a PRIOR iteration and merely stays mbOwned across the gap.
// Tracing agent.go confirms nothing sits between submit() returning
// (mb.state already mbOwned, mb.current.cancel still its zero value nil)
// and the loop's first testLoopRearmSeam call except OS-lock acquisition
// (session.TryAcquireSessionLock), which never touches the mailbox — so the
// FIRST firing of testLoopRearmSeam is, in every mailbox-observable respect,
// the exact same shape as the SECOND-or-later firings this file's other
// tests exercise: state mbOwned, current.cancel nil, dispatcherCancel ==
// runCancel. This test drives that first firing directly instead of relying
// on that trace alone.
//
// This test's FIRST draft assumed "there is no stale call here: nothing can
// be queued in mb.submitted before the session's very first owner exists" —
// that assumption was checked, not trusted, by running the test, and it was
// WRONG: reclaimReplacementOrKeep's requeue fix (the closing-review fix for
// the #283/P0-2-class discard defect) treats the loop's own `call` local —
// turn 1's original, about-to-run prompt — exactly the same way it treats
// an inter-turn `call` popped from mb.submitted: it gets pushed onto
// mb.submitted and requeued behind the replacement, then runs as turn 2.
// There is nothing special about "the original prompt was never queued
// anywhere" that exempts it — reclaimReplacementOrKeep doesn't know or care
// where `call` came from. This is CORRECT behavior, consistent with the
// inter-turn window and with the same "nothing the caller submitted is
// silently lost" contract driving the whole closing-review fix — the
// assertions below reflect it: two provider calls (replacement, then the
// original prompt), not one.
func TestRun_InterruptBeforeFirstGeneration_ReplacementRunsExactlyOnce(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "307 pre-first-generation interrupt test")
	require.NoError(t, err)

	var promptsMu sync.Mutex
	var promptsSeen []string
	requestStarted := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		promptsMu.Lock()
		promptsSeen = append(promptsSeen, string(body))
		promptsMu.Unlock()

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

	// Arm testLoopRearmSeam to park on its FIRST firing — turn 1's own loop
	// entry, before ANY beginGeneration has ever run for this session. This
	// is the window under test: mb.state is already mbOwned (set inside
	// submit(), which already returned by the time Run() reaches its loop),
	// but mb.current.cancel is still its zero value (nil) — nobody has
	// called beginGeneration yet.
	loopRearmEntered := make(chan struct{})
	releaseLoopRearm := make(chan struct{})
	var loopRearmCalls atomic.Int64
	mb.testLoopRearmSeam = func() {
		if loopRearmCalls.Add(1) == 1 {
			close(loopRearmEntered)
			<-releaseLoopRearm
		}
	}

	const originalPrompt = "ORIGINAL - turn 1's own prompt, requeued and must run SECOND, not lost"
	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          originalPrompt,
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Wait for the loop to park at its FIRST testLoopRearmSeam call —
	// strictly after submit() returned (mb.state already mbOwned) and
	// strictly before this iteration's own beginGeneration call.
	select {
	case <-loopRearmEntered:
	case err := <-runDone:
		t.Fatalf("Run returned before the loop-rearm seam was reached: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never reached testLoopRearmSeam for the first time")
	}

	// Fire InterruptAndReplace from a separate goroutine while the loop is
	// parked, and wait for it to return.
	const replacementPrompt = "REPLACEMENT - must pre-empt the original and run FIRST"
	interruptDone := make(chan bool, 1)
	go func() {
		hadOwner := sa.InterruptAndReplace(sess.ID, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          replacementPrompt,
			MaxOutputTokens: 1000,
		})
		interruptDone <- hadOwner
	}()

	var hadOwner bool
	select {
	case hadOwner = <-interruptDone:
	case <-time.After(10 * time.Second):
		t.Fatal("InterruptAndReplace never returned while the loop was parked at testLoopRearmSeam")
	}
	require.True(t, hadOwner, "InterruptAndReplace must report an owner existed — mb.state is already mbOwned by "+
		"the time Run() reaches its loop, even though no generation has ever been live yet")

	// Only now let the loop proceed to beginGeneration for turn 1.
	close(releaseLoopRearm)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "Run must complete successfully: the interrupt landing before the first "+
			"generation must NOT poison runCtx via a dispatcherCancel fallback — the same #307 mechanism as the "+
			"inter-turn window, just one iteration earlier")
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned — the dispatcher is wedged")
	}

	promptsMu.Lock()
	defer promptsMu.Unlock()
	require.Len(t, promptsSeen, 2, "exactly two provider calls must have been made: the replacement (pre-empting, "+
		"as turn 1) and then the original prompt, requeued as turn 2 — the original must NOT be discarded just "+
		"because it was never a `call` popped out of mb.submitted; reclaimReplacementOrKeep requeues it exactly "+
		"like any other pre-empted `call`")
	assert.Contains(t, promptsSeen[0], replacementPrompt, "the FIRST provider call must carry the REPLACEMENT "+
		"prompt — it pre-empts the original, which is the whole point of interrupt-and-replace")
	assert.Contains(t, promptsSeen[1], originalPrompt, "the SECOND provider call must carry the original prompt — "+
		"requeued, not lost")

	assert.False(t, sa.IsSessionBusy(sess.ID), "the session must not be left wedged busy after both turns complete")
}
