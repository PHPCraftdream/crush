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

// TestInterruptAndReplace_ReplacementReachesNextTurn_P0_2 is the mandatory
// end-to-end regression test for release-blocker P0-2
// (docs/reviews/2026-08-04-multi-agent-stability-follow-up.md).
//
// The defect: coordinator.InterruptAndSend did
//
//	c.currentAgent.QueueMessage(call)
//	c.currentAgent.Cancel(sessionID)
//
// and sessionAgent.Cancel unconditionally ran messageQueue.Clear(sessionID)
// — so the replacement was queued and then deleted by the very next line, on
// EVERY call, deterministically. Not a race: ordinary production flow. The
// web UI's "interrupt and send" button and `crush sessions inject
// --interrupt` silently discarded the user's new request.
//
// The pre-existing coordinator-level test only asserted call ORDER against a
// mock whose Cancel did not clear the queue, so it could never catch this.
// This test therefore drives the REAL sessionAgent (no mock): it interrupts a
// genuinely in-flight turn mid-stream and asserts the replacement actually
// runs as the next turn.
//
// Deterministic, not timing-dependent: the fake provider signals
// requestStarted only once turn 1's stream has genuinely begun, and the
// interrupt is fired from that signal — so the interrupt always lands
// mid-stream, never before the turn exists or after it finished.
func TestInterruptAndReplace_ReplacementReachesNextTurn_P0_2(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()
	var calls atomic.Int64

	sess, err := env.sessions.Create(t.Context(), "interrupt and replace test")
	require.NoError(t, err)

	var sa *sessionAgent
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)

	const replacementPrompt = "stop — do the replacement instead"

	// Fire the interrupt from turn 1's own stream-started signal, so it is
	// guaranteed to land while that turn is genuinely in flight.
	interruptDone := make(chan bool, 1)
	go func() {
		select {
		case <-requestStarted:
		case <-time.After(10 * time.Second):
			interruptDone <- false
			return
		}
		interrupted := sa.InterruptAndReplace(sess.ID, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          replacementPrompt,
			MaxOutputTokens: 1000,
		})
		interruptDone <- interrupted
		// Let the (now cancelled) first stream's handler return so the
		// test server goroutine doesn't leak.
		close(proceed)
	}()

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	// Separate, uncounted server for the small model: turn 1 always fires
	// generateTitle in the background on a session's first turn, and that
	// traffic must not pollute `calls`.
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
	sa = agentIface.(*sessionAgent)

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
	case <-runDone:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return within 20s after the interrupt — the replacement drain may have deadlocked")
	}

	select {
	case interrupted := <-interruptDone:
		require.True(t, interrupted,
			"InterruptAndReplace must report it actually interrupted a live turn (the session was genuinely mid-stream)")
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt goroutine never reported")
	}

	// THE core assertion. Turn 1 was cancelled mid-stream; the replacement
	// must then have run as turn 2. Before the fix, Cancel wiped the
	// replacement out of the queue and Run simply ended after turn 1 — so
	// this stays at 1 and the user's message is gone forever.
	assert.Equal(t, int64(2), calls.Load(),
		"expected two provider calls: the interrupted turn 1, then turn 2 running the replacement. "+
			"One call means the replacement was discarded — the P0-2 defect")

	// Prove it was OUR replacement that ran, not some other drained call:
	// the next turn persists its prompt as a user message.
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
		"the replacement prompt must have been persisted as a user message by the turn that ran it — "+
			"its absence means the replacement never reached a turn")

	// Nothing may be left stranded in either queue afterward.
	assert.Equal(t, 0, sa.QueuedPrompts(sess.ID),
		"no queued prompt may be left behind once Run returns")
}
