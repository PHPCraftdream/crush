package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_LateInterruptReplacement_SurvivesNormalCompletion_P0A is the
// end-to-end regression test for release-blocker P0-A (task #292, fixed by
// commit db546653).
//
// The bug: interruptAndReplace records the payload in mb.replacement and
// returns a cancel func for the caller to invoke outside the mailbox lock.
// If that cancel loses the race against agent.Stream's own normal
// completion — Stream returns nil, NOT context.Canceled, because the
// provider had already finished sending its last chunk by the time the
// cancel actually propagated — runTurn takes the normal-completion path
// instead of the isCancelErr branch. Only isCancelErr's branch called
// drainAfterCancel, which is the ONLY place that used to look at
// mb.replacement; the normal-completion path went straight to
// drainOrReleaseMerged/drainOrReleaseFinal, which (pre-fix) checked only
// mb.submitted and the legacy queue — never mb.replacement. The replacement
// was silently stranded: Run's own defer could only push it to the legacy
// queue (no runner) or no-op it under a new era.
//
// This test reproduces the exact race deterministically instead of relying
// on OS/network-level timing (calling the real cancel func and hoping the
// SSE response wins the race would make the test flaky in either direction
// — cancelling genCtx normally DOES propagate to the in-flight HTTP request
// and usually surfaces as context.Canceled, which is the common case this
// bug does NOT cover). Instead it drives the REAL production
// interruptAndReplace method directly (the same technique
// run_orphaned_release_race_test.go's full-path test uses for the
// analogous release-window race) to land mb.replacement, WITHOUT invoking
// the cancel func it returns — standing in for "the cancel lost the race
// and never actually stopped Stream in time". The ORIGINAL turn's stream is
// then allowed to complete normally by closing `proceed`, so runTurn's err
// is nil and the normal-completion path (not isCancelErr's drainAfterCancel)
// is what must recover the replacement. That is precisely the scenario
// drainOrReleaseFinal's own doc describes: "a late interrupt whose cancel
// lost the race to Stream's normal completion".
//
// Verified by revert: reverting db546653's replacement branch in
// drainOrReleaseFinal makes this test fail with exactly 1 provider call (the
// original turn only) and the replacement prompt absent from history — see
// the task report for the captured failure output.
func TestRun_LateInterruptReplacement_SurvivesNormalCompletion_P0A(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()
	var calls atomic.Int64

	sess, err := env.sessions.Create(t.Context(), "late interrupt replacement P0-A")
	require.NoError(t, err)

	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)

	const replacementPrompt = "replacement that raced the original completion, P0-A"

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

	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "original turn, about to race a late interrupt",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Wait until the original turn is demonstrably mid-stream (past
	// PrepareStep, past beginGeneration — mb.current.cancel is now the live
	// per-turn genCtx cancel, exactly the state interruptAndReplace needs to
	// find "a turn in progress").
	select {
	case <-requestStarted:
	case r := <-runDone:
		t.Fatalf("Run returned before the original turn reached the provider (err=%v)", r)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the original turn to reach the provider")
	}

	// Land mb.replacement via the REAL production mailbox method — the exact
	// call coordinator.InterruptAndSend makes for a live turn — WITHOUT
	// invoking the cancel func it returns. This deterministically reproduces
	// "the cancel lost the race to Stream's normal completion": mb.replacement
	// is durably recorded exactly as it would be in production, but genCtx is
	// never actually cancelled, so the turn's stream is free to complete on
	// its own via the SSE server below.
	mb := sa.getMailbox(sess.ID)
	_, hadOwner := mb.interruptAndReplace(SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          replacementPrompt,
		MaxOutputTokens: 1000,
	})
	require.True(t, hadOwner, "interruptAndReplace must report a live turn to interrupt — "+
		"the original turn was demonstrably mid-stream (requestStarted already fired)")

	// Let the original turn's stream finish delivering its final chunk and
	// close normally — runTurn's agent.Stream call returns err == nil, so
	// runTurn takes the normal-completion path (NOT the isCancelErr branch),
	// which is exactly what the P0-A defect failed to handle.
	close(proceed)

	// The replacement must reach the provider as turn 2 — proving
	// drainOrReleaseFinal recovered mb.replacement instead of stranding it.
	select {
	case <-requestStarted:
		// A second signal on requestStarted means a second request landed —
		// the replacement.
	case r := <-runDone:
		t.Fatalf("Run returned (err=%v) before the replacement ever reached the provider — "+
			"the P0-A defect: mb.replacement was recorded but the normal-completion drain never looked at it, "+
			"stranding the replacement with no runner", r)
	case <-time.After(10 * time.Second):
		t.Fatalf("replacement never reached the provider within 10s (calls=%d) — P0-A: replacement stranded", calls.Load())
	}

	// Let the replacement's own stream complete too (requestStartedSSEServer
	// blocks EVERY request on `proceed`, so the second invocation needs its
	// own unblock — but `proceed` is already closed, so a closed channel
	// receive returns immediately for the replacement's own block point).

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "Run must complete cleanly once both turns have run")
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s after the replacement started")
	}

	// THE core assertion: exactly 2 provider calls landed (original + the
	// recovered replacement). 1 means the replacement was stranded (P0-A
	// defect); the mailbox never handed it to a runner.
	assert.Equal(t, int64(2), calls.Load(),
		"expected exactly 2 provider calls: the original turn plus the recovered replacement. "+
			"1 means the replacement was stranded by the P0-A defect (mb.replacement ignored by the "+
			"normal-completion drain)")

	// The replacement prompt must have been persisted and actually run.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var foundReplacement bool
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == replacementPrompt {
			foundReplacement = true
			break
		}
	}
	assert.True(t, foundReplacement,
		"the replacement prompt must have been persisted and run as turn 2 — P0-A: it was silently "+
			"stranded in mb.replacement (or dropped into the legacy queue with no runner) instead")

	// Nothing may be left stranded in either queue once Run returns.
	assert.Equal(t, 0, sa.QueuedPrompts(sess.ID), "no queued prompt may be left behind once Run returns")
}
