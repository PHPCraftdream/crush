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
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingFirstPreambleGetSessionService wraps session.Service and blocks
// the FIRST Get call that carries a deadline (i.e. runTurn's preambleCtx —
// the only Get call site with a timeout). Subsequent calls pass through
// untouched. This pins the first turn inside its DB preamble so a test can
// deterministically fire InterruptAndReplace in that exact window.
type blockingFirstPreambleGetSessionService struct {
	session.Service
	entered chan struct{}
	fired   atomic.Bool
}

func (b *blockingFirstPreambleGetSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return b.Service.Get(ctx, id)
	}
	if !b.fired.Swap(true) {
		close(b.entered)
		<-ctx.Done()
		return session.Session{}, ctx.Err()
	}
	return b.Service.Get(ctx, id)
}

// TestRun_InterruptDuringFirstPreamble_DispatcherSurvives_284 is the
// regression test for #284 (Mailbox stage 2.3): an InterruptAndReplace
// that arrives during the VERY FIRST turn's DB preamble — before runTurn
// has created genCtx — must not kill the dispatcher.
//
// Before the fix, Run registered runCancel (the whole-dispatcher cancel)
// as mb.current.cancel for the preamble window. InterruptAndReplace
// cancelled runCancel, unwinding the entire turn loop. The replacement was
// recorded in the mailbox but the dispatcher that was supposed to drain it
// was already dead — the message ended up in the legacy queue with no
// runner, silently lost.
//
// The fix gives the preamble its own per-turn cancel (turnCancel, derived
// from runCtx), so cancelling the preamble leaves the dispatcher alive to
// atomically recover and run the replacement.
//
// Deterministic: the fake session.Service blocks the first preamble's Get
// call, and the interrupt is fired from that block. The test MUST fail
// without the fix (verified by reverting and running).
func TestRun_InterruptDuringFirstPreamble_DispatcherSurvives_284(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()
	var calls atomic.Int64

	sess, err := env.sessions.Create(t.Context(), "interrupt during preamble #284")
	require.NoError(t, err)

	var sa *sessionAgent
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)

	const replacementPrompt = "replacement during preamble #284"

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	// Separate, uncounted server for title generation (fires on turn 1).
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

	// Wrap sessions so the FIRST preamble Get blocks.
	blocking := &blockingFirstPreambleGetSessionService{
		Service: sa.sessions,
		entered: make(chan struct{}),
	}
	sa.sessions = blocking

	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "first message",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Wait until the first preamble is demonstrably blocked inside Get.
	select {
	case <-blocking.entered:
	case r := <-runDone:
		t.Fatalf("Run returned before the preamble was blocked (err=%v)", r)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the first preamble to block")
	}

	// Fire InterruptAndReplace while the preamble is blocked. This is the
	// exact seam #284 targets: before genCtx exists, the mailbox's
	// current.cancel is the per-turn cancel (after the fix) or runCancel
	// (before the fix).
	interrupted := sa.InterruptAndReplace(sess.ID, SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          replacementPrompt,
		MaxOutputTokens: 1000,
	})
	require.True(t, interrupted,
		"InterruptAndReplace must report it interrupted a live turn — the session was genuinely mid-preamble")

	// The per-turn cancel fired, the blocked Get returned context.Canceled,
	// runTurn recovered the replacement via drainAfterCancel, and the loop
	// continued. The replacement's preamble now runs (passes through the
	// blocking service) and reaches the provider.
	select {
	case <-requestStarted:
		// Replacement reached the provider.
	case r := <-runDone:
		t.Fatalf("Run returned before the replacement reached the provider (err=%v) — the dispatcher was killed by the interrupt (#284 defect)", r)
	case <-time.After(15 * time.Second):
		r := <-runDone
		t.Fatalf("replacement never reached the provider within 15s (calls=%d, runErr=%v) — the dispatcher was killed by the interrupt (#284 defect)", calls.Load(), r)
	}

	// Let the replacement's stream complete.
	close(proceed)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr,
			"Run must complete cleanly — the dispatcher survived the interrupt and ran the replacement")
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return within 20s")
	}

	// THE core assertion: exactly ONE provider call (the replacement).
	// 0 = dispatcher was killed, replacement stranded.
	// 2+ = replacement ran more than once.
	assert.Equal(t, int64(1), calls.Load(),
		"expected exactly 1 provider call: the replacement running once. "+
			"0 means the dispatcher was killed by the preamble interrupt and the replacement was stranded (#284); "+
			"2+ means the replacement ran more than once")

	// The replacement prompt must have been persisted as a user message.
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
		"the replacement prompt must have been persisted as a user message by the turn that ran it")

	// Nothing may be left stranded in either queue.
	assert.Equal(t, 0, sa.QueuedPrompts(sess.ID),
		"no queued prompt may be left behind once Run returns")
}
