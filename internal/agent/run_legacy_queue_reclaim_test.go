package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_LegacyQueueMessageDuringFinalDrain_DoesNotWedgeAcrossTurns is the
// end-to-end regression test for round 10 review, HIGH-1.
//
// drainOrReleaseMerged's legacy-messageQueue fallback used to reclaim
// ownership via a plain mailbox.submit() call. submit() is the ONLY
// idle->owned transition and therefore ALWAYS bumps the mailbox's ownership
// epoch — but the reclaimed epoch was discarded (`became, _ :=
// mb.submit(...)`), and Run's loop had already captured its own `epoch`
// exactly once at claim time (agent.go:849), never re-reading it. So turn
// 2 (running the reclaimed legacy-queue message) executed under a NEWER
// epoch than the one Run's loop believed it held. Turn 2's own end-of-turn
// drain (or, if turn 2 also found nothing new, Run's cleanup defer)
// presented the STALE original epoch, found it no longer matched, assumed
// a different owner now held the session, and did nothing — permanently
// re-wedging exactly the shape BLOCKER-1/BLOCKER-2 already closed, via a
// narrower trigger: any message landing in the legacy queue in the window
// between a turn's own release-to-idle and its check of that queue.
//
// This test reproduces that window deterministically: it queues directly
// into sessionAgent.messageQueue (simulating a legacy QueueMessage caller,
// e.g. InterruptAndSend's idle-session fallback) strictly after turn 1's
// own PrepareStep has already run (proven by waiting for the request to
// actually reach the provider) but before turn 1's stream completes — so
// turn 1 cannot fold it into its own prompt, and it can only be picked up
// by the end-of-turn drain's legacy-queue fallback.
func TestRun_LegacyQueueMessageDuringFinalDrain_DoesNotWedgeAcrossTurns(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "high-1 legacy queue reclaim test")
	require.NoError(t, err)

	var calls atomic.Int64
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
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
			Prompt:          "first message",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1 never reached the provider")
	}

	// Land directly in the legacy queue, simulating a caller that hasn't
	// (or, for InterruptAndSend/requeueInterruptMessage's idle-session
	// fallback, never will) gone through the mailbox's own submit path.
	sa.messageQueue.Append(sess.ID, SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued via the legacy path during turn 1's stream",
		MaxOutputTokens: 1000,
	})

	close(proceed) // let turn 1 (and every subsequent turn on this server) finish

	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}

	assert.Equal(t, int64(2), calls.Load(),
		"expected two provider calls: turn 1, then the legacy-queued message reclaimed as turn 2")

	// THE core HIGH-1 assertion.
	assert.False(t, sa.IsSessionBusy(sess.ID),
		"the session must be idle once both turns complete — before the fix, reclaiming from the legacy "+
			"queue bumped the mailbox's epoch out from under Run's loop (which never re-reads it), so turn 2's "+
			"own end-of-turn release (or Run's cleanup defer) presented a now-stale epoch, found a mismatch, "+
			"assumed a different owner held the session, and did nothing — permanently wedging it busy")

	// Prove it's not wedged, not just that the flag happens to read false:
	// a THIRD Run() call for the same session must actually run.
	res, err := agentIface.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "third message, session must still be claimable",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	assert.NotNil(t, res, "a wedged session would silently queue this and Run would return (nil, nil) without ever calling the provider")
	assert.Equal(t, int64(3), calls.Load())
}
