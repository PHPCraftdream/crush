package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingSecondGetSessionService's Get blocks only on calls made with a
// context that has a deadline — i.e. runTurn's preambleCtx
// (context.WithTimeout(ctx, sessionPreambleMaxDuration)), the ONLY call site
// among the several a.sessions.Get call sites in agent.go that carries a
// deadline. Every other call site (OnStepFinish's mid-turn re-fetch, the
// summarize helpers, etc.) derives its ctx from genCtx via plain
// context.WithCancel with no deadline, so those pass through untouched.
// This matters because runTurn calls a.sessions.Get more than once per
// call to Run — critically, OnStepFinish (agent.go ~1712) re-fetches the
// session once per completed step, which already happens during turn 1
// itself. A naive "block on the 2nd call overall" counter would trip
// inside turn 1's own OnStepFinish instead of turn 2's DB preamble, which
// is useless for this test: pinning down the preambleCtx deadline is what
// actually isolates "a turn's DB preamble" calls from every other Get call.
//
// The first preamble call (turn 1's own) passes through untouched — turn 1
// must complete normally so a second, queue-drained turn actually starts.
// The second preamble call (turn 2's) blocks and signals entered once,
// pinning down the exact window task #206 is about: after turn 1 has fully
// returned (its own per-turn `cancel` already spent) and before turn 2's
// runTurn has reached its own `a.activeRequests.Set(call.SessionID,
// cancel)` — i.e. while activeRequests still holds whatever Run's turn
// loop set it to.
type blockingSecondGetSessionService struct {
	session.Service
	preambleCalls atomic.Int64
	entered       chan struct{}
	unblock       chan struct{}
}

func (b *blockingSecondGetSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		// Not a preamble call (e.g. OnStepFinish's mid-turn re-fetch) —
		// pass straight through.
		return b.Service.Get(ctx, id)
	}
	n := b.preambleCalls.Add(1)
	if n < 2 {
		return b.Service.Get(ctx, id)
	}
	if n == 2 {
		close(b.entered)
	}
	select {
	case <-b.unblock:
	case <-ctx.Done():
		return session.Session{}, ctx.Err()
	}
	return b.Service.Get(ctx, id)
}

// TestRun_CancelDuringSecondTurnPreamble_ActuallyCancels is the regression
// test for task #206: the turn loop in Run() only registered runCancel (the
// whole-call CancelFunc tryReserveSession stores in activeRequests) once,
// before the loop started. runTurn itself later overwrites that map entry
// with its own per-turn `cancel` (derived from runCtx) partway through the
// turn — but once that turn returns, its `cancel` has already been invoked
// via defer and is dead. From the moment one turn returns until the next
// queue-drained turn's runTurn reaches its own activeRequests.Set — i.e.
// for the whole of the next turn's DB preamble, bounded by
// sessionPreambleMaxDuration — activeRequests pointed at that dead
// placeholder. A Cancel(sessionID) landing in that window found the dead
// func, called it, and nothing happened: the turn that was actually
// executing (stuck in its DB preamble) kept right on going, exactly the
// "dead placeholder silently no-op'ing" tryReserveSession's own doc comment
// says must not happen.
//
// This test drives Run() through two turns via the same queue-drain path
// TestRun_QueueDrain_DoesNotDeadlockOnOwnSessionLock uses (second message
// queued mid-stream, after PrepareStep's TakeAll for turn 1, so it runs as
// a genuinely separate turn 2 through the same loop rather than being
// folded into turn 1's own prompt). A fake session.Service blocks turn 2's
// DB preamble (its second Get call) and signals entered the moment it does
// — i.e. exactly inside the window described above, before turn 2's runTurn
// has replaced the map entry with its own live cancel func. From there the
// test calls the public Cancel(sessionID) and asserts Run() actually
// returns with a cancellation-shaped error instead of hanging or
// completing normally, proving the registered CancelFunc was live (and
// therefore actually interrupted turn 2's preamble) rather than a spent
// no-op.
func TestRun_CancelDuringSecondTurnPreamble_ActuallyCancels(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()
	var calls atomic.Int64

	sess, err := env.sessions.Create(t.Context(), "cancel between turns test")
	require.NoError(t, err)

	var a SessionAgent
	var sa *sessionAgent
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)

	// Queue the second message once turn 1's stream has begun (i.e. after
	// PrepareStep's TakeAll for turn 1 has already run), same as
	// TestRun_QueueDrain_DoesNotDeadlockOnOwnSessionLock — this guarantees
	// the second message is drained as its own turn 2 through the turn
	// loop, not folded into turn 1's prompt.
	go func() {
		select {
		case <-requestStarted:
		case <-time.After(5 * time.Second):
			return
		}
		sa.getMailbox(sess.ID).queue(SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "second message",
			MaxOutputTokens: 1000,
		})
		close(proceed)
	}()

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

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	smallModel := Model{
		Model:      titleLM,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}
	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           smallModel,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	a = agentIface
	sa = a.(*sessionAgent)

	blocking := &blockingSecondGetSessionService{
		Service: sa.sessions,
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	sa.sessions = blocking

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

	// Wait until turn 2 is demonstrably inside its DB preamble (its second
	// Get call), i.e. past turn 1's return and before turn 2's own
	// activeRequests.Set — the exact window task #206 closes.
	select {
	case <-blocking.entered:
	case r := <-resultCh:
		t.Fatalf("Run returned before turn 2 reached its DB preamble (err=%v) — cannot exercise the between-turns window", r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for turn 2 to reach its DB preamble")
	}

	// Cancel while turn 2 is blocked in its preamble. With the fix,
	// activeRequests[sess.ID] still holds the live runCancel at this point,
	// so this must actually unblock (via ctx.Done()) the fake Get call
	// above and cause Run() to return with a cancellation error — not hang
	// until unblock is separately closed, and not silently no-op.
	sa.Cancel(sess.ID)

	select {
	case r := <-resultCh:
		require.Error(t, r.err, "Cancel(sessionID) during turn 2's DB preamble must surface as an error from Run(), not a silent no-op")
		assert.True(t, errors.Is(r.err, context.Canceled),
			"expected turn 2's preamble to fail with context.Canceled once runCancel actually fired, got %v", r.err)
	case <-time.After(5 * time.Second):
		close(blocking.unblock) // unstick the fake so the goroutine doesn't leak past the test
		t.Fatal("Run did not return after Cancel(sessionID) during turn 2's DB preamble — the registered CancelFunc was a dead placeholder (task #206 regression)")
	}

	// First turn must have reached the provider; second turn's provider
	// call never happens because it was cancelled while still in its DB
	// preamble, before genCtx (and therefore the provider request) existed.
	assert.Equal(t, int64(1), calls.Load(),
		"turn 2 must have been cancelled before ever reaching the provider")
}
