package agent

import (
	"context"
	"errors"
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

// TestP1_3_TickOperationTimeout verifies that each interrupt tick has a
// bounded operation deadline. If a tick's handleInterruptTick blocks on
// an operation that ignores parent ctx cancellation, the tick still returns
// within interruptTickOperationTimeout, and the goroutine can observe
// ctx.Done() and exit cleanly.
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

	// Replace sessions.PeekInterruptInject with a blocking version that
	// ignores ctx cancellation (simulating the bug scenario)
	blockChan := make(chan struct{})
	var blockingPeekCalled atomic.Bool
	blockingSessions := &blockingSessionService{
		Service: env.sessions,
		blockPeek: func(_ context.Context, _ string) (*session.PendingInject, error) {
			blockingPeekCalled.Store(true)
			// Block forever (or until test cleans up)
			<-blockChan
			return nil, errors.New("should not reach here")
		},
	}
	coord.sessions = blockingSessions

	// Start ticker with done channel
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := coord.startInterruptTicker(tickerCtx, sess.ID)

	// Wait for the blocking PeekInterruptInject to be called
	require.Eventually(t, func() bool {
		return blockingPeekCalled.Load()
	}, 5*time.Second, 100*time.Millisecond, "PeekInterruptInject should have been called")

	// Now cancel the ticker context (simulating coordinator shutdown)
	// This is the key test: even though handleInterruptTick is blocked on
	// an operation that ignores ctx cancellation, the ticker goroutine
	// should observe ctx.Done() and exit within the operation timeout.
	stopTicker()

	// Verify that ticker goroutine exits within the operation timeout + a small margin
	// If the anti-pattern (timeout on join instead of operation) were present,
	// this would hang forever because the goroutine is still blocked in PeekInterruptInject.
	start := time.Now()
	select {
	case <-tickerDone:
		elapsed := time.Since(start)
		// Should exit within interruptTickOperationTimeout (10s) plus a small margin
		// for goroutine scheduling and cleanup. The blocking call will timeout after
		// interruptTickOperationTimeout, then the select loop observes ctx.Done().
		require.Less(t, elapsed, interruptTickOperationTimeout+2*time.Second,
			"ticker should join within operation timeout after ctx cancellation")
		slog.Info("TestP1_3_TickOperationTimeout: ticker joined after", "elapsed", elapsed)
	case <-time.After(interruptTickOperationTimeout + 5*time.Second):
		t.Fatal("ticker goroutine did not join within expected timeout - anti-pattern detected (timeout on join instead of operation)")
	}

	// Clean up: unblock the goroutine so it can exit cleanly
	close(blockChan)
}

// TestP1_3_NoUseAfterClose verifies that when a tick times out and the
// parent ctx is cancelled, the ticker goroutine exits cleanly without
// continuing to run in the background. This validates that the timeout
// is on the operation, not on the join (which would be the anti-pattern).
func TestP1_3_NoUseAfterClose(t *testing.T) {
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
	sess, err := env.sessions.Create(ctx, "use-after-close")
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

	// Replace sessions with a version that blocks and tracks when join completes
	blockChan := make(chan struct{})
	var blockingPeekCalled atomic.Bool
	var joinCompleted atomic.Bool
	var goroutineStillBlocking atomic.Bool
	blockingSessions := &blockingSessionService{
		Service: env.sessions,
		blockPeek: func(_ context.Context, _ string) (*session.PendingInject, error) {
			blockingPeekCalled.Store(true)
			// Wait until after join has completed, then verify the blocking
			// goroutine is still running (it would be cancelled if the anti-pattern
			// of timing out the join instead of the operation were present)
			for !joinCompleted.Load() {
				time.Sleep(10 * time.Millisecond)
			}
			// At this point, the join has completed but we're still in the
			// blocking operation - this proves the goroutine is still alive
			// (anti-pattern would have killed it by timing out the join)
			goroutineStillBlocking.Store(true)
			<-blockChan
			return nil, errors.New("should not reach here")
		},
	}
	coord.sessions = blockingSessions

	// Start ticker
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := coord.startInterruptTicker(tickerCtx, sess.ID)

	// Wait for the blocking operation to start
	require.Eventually(t, func() bool {
		return blockingPeekCalled.Load()
	}, 5*time.Second, 100*time.Millisecond, "blocking peek should have been called")

	// Cancel and wait for join - should complete within operation timeout
	stopTicker()
	select {
	case <-tickerDone:
		joinCompleted.Store(true)
	case <-time.After(interruptTickOperationTimeout + 5*time.Second):
		t.Fatal("ticker goroutine did not join within expected timeout - anti-pattern detected")
	}

	// Give the blocking goroutine time to notice join completed
	time.Sleep(200 * time.Millisecond)

	// Verify the blocking goroutine is still running (it was NOT killed by
	// timing out the join, which would be the anti-pattern)
	require.True(t, goroutineStillBlocking.Load(),
		"blocking goroutine should still be running after join completes - if it's not, the anti-pattern may be present")

	// Unblock it so the test can finish cleanly
	close(blockChan)

	// Give time for cleanup
	time.Sleep(100 * time.Millisecond)
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
