package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestActiveRequests_HoldsLiveCancelDuringTurn is the regression test for
// BUG-4 (full-project reviewer audit, 2026-08-11): the max-cost / max-tokens
// abort paths in OnStepFinish (agent.go ~2578-2604) — and the peak-hours and
// DB-cancel paths alongside them — stop the turn ONLY by calling the
// cancelFunc looked up from activeRequests. Returning an error from
// OnStepFinish alone does NOT break fantasy's loop (see the peak-hours
// comment at agent.go ~2632: "returning an error from OnStepFinish alone
// doesn't stop it"). If activeRequests.Get returned ok=false at that point,
// the cancel would be skipped and the agent would keep burning tokens/cost
// past the cap with no effect on the running turn.
//
// That is only safe today because of two non-obvious invariants:
//  1. runTurn stores the turn's genCtx cancel via activeRequests.Set before
//     the agent.Stream call whose OnStepFinish callback looks it up — so the
//     entry is always present by the time OnStepFinish can fire.
//  2. Nothing ever calls activeRequests.Del for the plain sessionID key
//     (releaseSessionReservation now routes through the mailbox and no
//     longer touches activeRequests — see IsBusy's doc in agent.go).
//     Entries live forever as already-fired, inert cancelFuncs.
//
// Should any future change reclaim an activeRequests entry before the turn
// ends (e.g. a later mailbox-migration stage), this test fails — preventing
// the budget-cap enforcement from silently degrading into a no-op.
func TestActiveRequests_HoldsLiveCancelDuringTurn(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "activeRequests BUG-4 test")
	require.NoError(t, err)

	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	releaseProceed := func() { proceedOnce.Do(func() { close(proceed) }) }

	srv := requestStartedSSEServer(nil, requestStarted, proceed)
	t.Cleanup(srv.Close)
	// Register releaseProceed AFTER srv.Close so that (LIFO) it runs BEFORE
	// srv.Close — otherwise the test server handler stays blocked on
	// <-proceed and srv.Close hangs.
	t.Cleanup(releaseProceed)
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
			Prompt:          "hello",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Wait until the generation is genuinely in flight — the server has
	// streamed its first delta and is now blocked holding the response
	// open. This is the window that contains every OnStepFinish invocation
	// for this turn.
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the provider")
	}

	// BUG-4 invariant: activeRequests MUST hold a non-nil cancel for this
	// session throughout the turn. ok=false here is exactly the silent
	// bypass the reviewer flagged — the budget-cap abort paths would skip
	// the cancelFn() call and only return an error, which fantasy ignores.
	cancelFn, ok := sa.activeRequests.Get(sess.ID)
	require.True(t, ok,
		"activeRequests must hold an entry for the in-flight session — "+
			"BUG-4: if ok is false, OnStepFinish's max-cost/max-tokens/peak-hours "+
			"abort paths silently no-op (return an error fantasy ignores) and the "+
			"turn runs past the cap")
	require.NotNil(t, cancelFn, "activeRequests cancel must be non-nil")

	// The entry must be THIS turn's live genCtx cancel, not a stale
	// already-fired one left over from a previous turn: invoking it must
	// actually interrupt the in-flight generation. If it were inert,
	// agent.Stream would stay blocked on the server's next chunk (which
	// only arrives after `proceed` is closed) and Run would NOT return on
	// its own within the timeout below.
	cancelFn()

	select {
	case <-runDone:
		// Run returned because the live cancel killed genCtx mid-stream —
		// the budget-cap abort path can in fact stop the turn.
	case <-time.After(10 * time.Second):
		t.Fatal("calling the activeRequests cancel did not interrupt the in-flight " +
			"turn within 10s — BUG-4 invariant violated: the stored cancel is " +
			"stale/inert (a no-op), so the budget-cap abort paths would fail to " +
			"stop the turn even though ok was true")
	}

	// Run has returned; release the server handler so httptest.Server.Close
	// (registered via t.Cleanup above) does not block on the handler still
	// waiting on <-proceed.
	releaseProceed()

	// The turn must have cleaned up after itself — mailbox back to idle,
	// matching every other turn-lifecycle test in this package.
	assert.False(t, sa.IsBusy(), "IsBusy must be false once the aborted turn has returned")
}
