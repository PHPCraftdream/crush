package agent

import (
	"context"
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

// TestP1_4_SequentialInterrupts verifies that two consecutive cross-process
// interrupts both interrupt their respective active generations. Before the
// fix, the ticker goroutine exited after the first interrupt, leaving the
// replacement turn (which runs under the same coordinator Run call) without
// interrupt coverage. A second interrupt would then remain pending and never
// interrupt the replacement turn.
//
// Regression for docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md
// P1-4: "interrupt ticker не соответствует lifecycle turn и не join-ится"
func TestP1_4_SequentialInterrupts(t *testing.T) {
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
	sess, err := env.sessions.Create(ctx, "sequential-interrupts")
	require.NoError(t, err)

	// Create first interrupt
	msg1, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt 1"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID, MessageID: msg1.ID, Content: "interrupt 1", Interrupt: true,
	}))

	// Create second interrupt (created after first, so it should be processed second)
	msg2, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt 2"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID, MessageID: msg2.ID, Content: "interrupt 2", Interrupt: true,
	}))

	// Simulate two consecutive ticks (as the ticker would do)
	// First tick should consume interrupt 1
	fired1, err := coord.handleInterruptTick(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, fired1, "first interrupt should fire")

	// Verify first interrupt was handled
	require.Len(t, agent.interruptAndReplaced, 1, "first interrupt should call InterruptAndReplace")
	require.Equal(t, msg1.ID, agent.interruptAndReplaced[0].ExistingMessageID)

	// Second tick should consume interrupt 2 (this is the regression: without the fix,
	// the ticker would have exited after the first interrupt, so this second tick
	// would never happen in a real scenario, but we're testing the handler directly here)
	fired2, err := coord.handleInterruptTick(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, fired2, "second interrupt should also fire")

	// Verify both interrupts were handled
	require.Len(t, agent.interruptAndReplaced, 2, "both interrupts should call InterruptAndReplace")
	require.Equal(t, msg2.ID, agent.interruptAndReplaced[1].ExistingMessageID)
}

// TestP1_4_TickerJoinsBeforeReturn verifies that the ticker goroutine
// joins before runInternal returns. Before the fix, stopTicker() only
// cancelled the context but did not wait for the goroutine to exit,
// meaning handleInterruptTick could still be executing after the caller
// had returned and potentially cleaned up DB resources.
func TestP1_4_TickerJoinsBeforeReturn(t *testing.T) {
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
	sess, err := env.sessions.Create(ctx, "ticker-join")
	require.NoError(t, err)

	// Start the ticker
	tickerCtx, stopTicker := context.WithCancel(ctx)

	// Create an interrupt to trigger handleInterruptTick
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt me"}},
	})
	require.NoError(t, err)
	require.NoError(t, env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID, MessageID: msg.ID, Content: "interrupt me", Interrupt: true,
	}))

	// Start ticker with done channel
	tickerDone := coord.startInterruptTicker(tickerCtx, sess.ID)

	// Give ticker time to process the interrupt (ticker ticks every 3 seconds)
	time.Sleep(4 * time.Second)

	// Now stop the ticker (this simulates runInternal returning)
	stopTicker()

	// Verify that ticker goroutine has exited by waiting on done channel
	// If the fix is incorrect (no join), this would block indefinitely or timeout
	select {
	case <-tickerDone:
		// Success: goroutine joined
	case <-time.After(5 * time.Second):
		t.Fatal("ticker goroutine did not join within timeout")
	}

	// Also verify the interrupt was handled
	require.Len(t, agent.interruptAndReplaced, 1, "interrupt should have been handled")
	require.Equal(t, msg.ID, agent.interruptAndReplaced[0].ExistingMessageID)
}
