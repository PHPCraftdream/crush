package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionAgent_IsBusy_FalseAfterTurnCompletes is the regression test for
// release-blocker BLOCKER-1 (round 9 review).
//
// releaseSessionReservation (mailbox.drainOrRelease, task #282) stopped
// clearing the plain-sessionID activeRequests entry once the mailbox
// migration landed — tryReserveSession/Run's loop still WRITE it every turn
// (activeRequests.Set), so after the first turn any session ever ran,
// activeRequests permanently held a non-nil (already-fired, inert)
// cancelFunc for it. IsBusy() read activeRequests directly and therefore
// returned true FOREVER after any session's first turn completed.
// CancelAll (called by App.Shutdown, reached by every `crush run` via
// `defer a.Shutdown()`) short-circuits on !IsBusy() to return immediately
// when genuinely idle; with the bug, it never could, so shutdown always
// burned the full 5-second drain timeout instead of returning at once.
//
// The pre-existing coordinator tests never caught this because they use a
// mockSessionAgent whose Cancel/IsBusy are trivial stubs, not the real
// mailbox-backed implementation. This test drives the REAL sessionAgent.
func TestSessionAgent_IsBusy_FalseAfterTurnCompletes(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "is-busy test")
	require.NoError(t, err)

	srv := singleTurnSSEServer(nil)
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

	require.False(t, sa.IsBusy(), "precondition: no turn has ever run, must not be busy")

	_, err = agentIface.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "hello",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)

	assert.False(t, sa.IsBusy(),
		"IsBusy must return false once the turn has completed and Run has returned — "+
			"BLOCKER-1: the pre-fix implementation read activeRequests directly, which is "+
			"never cleared for the plain sessionID key post-mailbox-migration, so this "+
			"would incorrectly stay true forever after the FIRST turn any session ran")

	// CancelAll must therefore return immediately, not burn the 5-second
	// drain timeout — this is what App.Shutdown relies on for every
	// `crush run` invocation.
	start := time.Now()
	sa.CancelAll()
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 1*time.Second,
		"CancelAll must return immediately when genuinely idle, not burn the 5s drain timeout (BLOCKER-1)")
}

// TestSessionAgent_IsBusy_TrueDuringLiveTurn is the non-regression
// companion: IsBusy must still correctly report true while a turn is
// genuinely in flight, proving the mailbox-based check (not just the
// removal of the stale activeRequests read) is wired correctly.
func TestSessionAgent_IsBusy_TrueDuringLiveTurn(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "is-busy live test")
	require.NoError(t, err)

	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(nil, requestStarted, proceed)
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

	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "hello",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the provider")
	}

	assert.True(t, sa.IsBusy(), "IsBusy must report true while a turn is genuinely in flight")

	close(proceed)
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}

	assert.False(t, sa.IsBusy(), "IsBusy must go back to false once the turn completes")
}
