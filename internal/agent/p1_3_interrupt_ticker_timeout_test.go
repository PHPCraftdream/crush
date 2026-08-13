package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP1_3_TickOperationTimeout verifies that each interrupt tick is bounded
// by interruptTickOperationTimeout INDEPENDENTLY of the outer ctx's own
// lifetime -- not just "eventually unblocks once the outer ctx is
// cancelled" (which a context-aware downstream call would already do with
// or without this fix, since tickCtx is a child of ctx and inherits its
// cancellation). The outer ctx here is deliberately never cancelled during
// the timing assertion: the only thing that can make handleInterruptTick
// return is its OWN operation-scoped deadline firing.
//
// HONEST LIMITATION (found by independent review, not assumed): this test
// -- and the fix itself -- can only bound a downstream call that is
// context-aware (checks ctx.Done() at all). context.WithTimeout cannot
// force-preempt a goroutine stuck in a call that never checks its context;
// Go has no such preemption primitive. What this fix actually delivers is
// a genuine, real improvement for a DIFFERENT case than "ignores
// cancellation": a downstream dependency that legitimately respects
// context but is merely SLOW (contended lock, slow disk, etc.) is now
// bounded to interruptTickOperationTimeout regardless of how long the
// outer, turn-scoped ctx happens to stay alive (previously: unbounded, up
// to the whole turn's duration). A dependency that truly never checks any
// context at all is not, and cannot be, addressed by this class of fix at
// all -- doing so would require running handleInterruptTick in a detached
// goroutine and abandoning it on timeout, which reintroduces exactly the
// use-after-close risk this whole task's anti-pattern warning exists to
// avoid (the abandoned goroutine could still be mid-write when a later
// caller tears down DB/resources). Not implemented here for that reason.
//
// Regression for docs/reviews/2026-08-13-release-readiness-static-audit.md
// P1-3: "join interrupt ticker-а не ограничен собственным deadline"
func TestP1_3_TickOperationTimeout(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	})
	cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}
	cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}

	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("ok"), nil
	})
	coord := &coordinator{
		cfg:          cfg,
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: agent,
		modelCache:   csync.NewMap[string, cachedModelPair](),
	}

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "tick-timeout")
	require.NoError(t, err)

	// Create an interrupt to trigger handleInterruptTick
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt me"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID, MessageID: msg.ID, Content: "interrupt me", Interrupt: true,
	}))

	// blockPeek genuinely (correctly) respects whatever ctx it's handed --
	// this is what a REAL context-aware dependency does. It never gets an
	// external unblock signal, so the ONLY way it returns is via its own
	// ctx.Done() firing. Passed tickCtx (bounded to
	// interruptTickOperationTimeout) by the fix -- if that bound were
	// removed and handleInterruptTick were called with the bare, never
	// -cancelled outer ctx instead, this would block for the rest of the
	// test's lifetime, which the revert-check below relies on.
	var timedOutAt atomic.Value // stores time.Time
	blockingSessions := &blockingSessionService{
		Service: env.sessions,
		blockPeek: func(ctx context.Context, _ string) (*session.PendingInject, error) {
			<-ctx.Done()
			timedOutAt.Store(time.Now())
			return nil, ctx.Err()
		},
	}
	coord.sessions = blockingSessions

	// Deliberately never cancelled during the timing assertion below --
	// the whole point is to prove the tick's bound does not depend on this
	// ctx's own cancellation.
	tickerCtx, stopTicker := context.WithCancel(ctx)
	t.Cleanup(stopTicker)
	start := time.Now()
	tickerDone := coord.startInterruptTicker(tickerCtx, sess.ID)

	// The ticker doesn't fire its first tick until interruptInjectTick has
	// elapsed (time.NewTicker doesn't fire immediately), and ONLY THEN does
	// handleInterruptTick start -- and with it, tickCtx's own
	// interruptTickOperationTimeout budget. So the total elapsed time from
	// `start` (before the ticker even begins) to the operation deadline
	// firing is interruptInjectTick + interruptTickOperationTimeout, not
	// interruptTickOperationTimeout alone -- an earlier version of this
	// test measured from `start` but bounded against
	// interruptTickOperationTimeout alone, which put the real value
	// (~13s = 3s tick interval + 10s operation timeout) right at the edge
	// of its own assertion, causing a few-millisecond-over failure on
	// ~30% of runs. Found via the orchestrator's own repeated-run
	// verification (a rewrite done in direct response to an independent
	// review's H3 finding), not by the review itself.
	wantElapsed := interruptInjectTick + interruptTickOperationTimeout
	require.Eventually(t, func() bool {
		return timedOutAt.Load() != nil
	}, wantElapsed+5*time.Second, 200*time.Millisecond,
		"the tick's own operation deadline should fire on its own, without the outer ctx ever being cancelled")

	elapsed := timedOutAt.Load().(time.Time).Sub(start)
	require.GreaterOrEqual(t, elapsed, wantElapsed,
		"should not fire before the tick interval plus the configured operation timeout")
	require.Less(t, elapsed, wantElapsed+3*time.Second,
		"should fire close to tick-interval+operation-timeout, not much later")
	slog.Info("TestP1_3_TickOperationTimeout: tick's own deadline fired after", "elapsed", elapsed)

	// Now confirm the ticker still joins cleanly once the outer ctx IS
	// cancelled (unrelated to the operation-timeout property above, but
	// cheap to confirm here rather than a separate test).
	stopTicker()
	select {
	case <-tickerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ticker goroutine did not join after ctx cancellation")
	}
}

// TestP1_3_SuccessfulTickNotImpacted verifies that a successful tick
// (one that completes quickly and without errors) is not impacted by
// the per-tick timeout — it should complete normally and the operation
// context should not expire.
func TestP1_3_SuccessfulTickNotImpacted(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	})
	cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}
	cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-model",
	}

	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("ok"), nil
	})
	coord := &coordinator{
		cfg:          cfg,
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: agent,
		modelCache:   csync.NewMap[string, cachedModelPair](),
	}

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "successful-tick")
	require.NoError(t, err)

	// Create an interrupt
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt me"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID, MessageID: msg.ID, Content: "interrupt me", Interrupt: true,
	}))

	// Start ticker
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := coord.startInterruptTicker(tickerCtx, sess.ID)

	// Wait for the interrupt to be processed (should happen within one tick
	// interval). Reads interruptAndReplaced through the mock's thread-safe
	// snapshot helper, NOT the raw field: the ticker goroutine is still
	// running (not yet joined via stopTicker/tickerDone below), so a plain
	// field read here would race with InterruptAndReplace's concurrent
	// write (caught by -race in isolated repeated runs).
	require.Eventually(t, func() bool {
		return len(agent.interruptAndReplacedSnapshot()) > 0
	}, interruptInjectTick+2*time.Second, 100*time.Millisecond, "interrupt should have been processed")

	// Verify the interrupt was handled correctly
	replaced := agent.interruptAndReplacedSnapshot()
	require.Len(t, replaced, 1, "interrupt should have been handled")
	require.Equal(t, msg.ID, replaced[0].ExistingMessageID)

	// Stop the ticker cleanly
	stopTicker()
	select {
	case <-tickerDone:
		// Success: goroutine joined cleanly
	case <-time.After(5 * time.Second):
		t.Fatal("ticker goroutine did not join within timeout")
	}
}

// blockingSessionService wraps session.Service and allows blocking
// PeekInterruptInject calls to simulate operations that ignore ctx cancellation.
// The key insight: we use a select with a separate timeout channel so the
// blocking can be observed even when the passed ctx is cancelled.
type blockingSessionService struct {
	session.Service
	blockPeek func(ctx context.Context, sessionID string) (*session.PendingInject, error)
}

func (s *blockingSessionService) PeekInterruptInject(ctx context.Context, sessionID string) (*session.PendingInject, error) {
	// Use a channel to control when this operation unblocks
	unblock := make(chan struct{})
	var result *session.PendingInject
	var err error

	go func() {
		// Run the actual blocking peek
		result, err = s.blockPeek(ctx, sessionID)
		close(unblock)
	}()

	select {
	case <-unblock:
		return result, err
	case <-ctx.Done():
		// Context was cancelled, but the goroutine is still running
		// Return immediately to the caller - the goroutine will continue
		// in the background and eventually complete
		return nil, ctx.Err()
	}
}
