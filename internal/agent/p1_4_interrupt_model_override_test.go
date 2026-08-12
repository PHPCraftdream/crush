package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestBuildCall_RequiresPinned ensures that buildCall rejects nil pinned.
// This is a compile-time enforcement of the P1-4 fix: all callers must
// resolve session models before calling buildCall, so cross-process paths
// like interrupt requeue respect session-scoped model overrides.
func TestBuildCall_RequiresPinned(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	const providerID = "test-provider"
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-large-model", Name: "Test Large", DefaultMaxTokens: 4096},
		},
	})
	cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: providerID,
		Model:    "test-large-model",
	}

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	coord.currentAgent = newMockAgent(providerID, 4096, nil)

	sess, err := env.sessions.Create(t.Context(), "test-session")
	require.NoError(t, err)

	// buildCall must return an error when called with pinned == nil.
	_, err = coord.buildCall(t.Context(), sess.ID, "prompt", nil, nil)
	require.Error(t, err, "buildCall must reject nil pinned after P1-4 fix")
	require.Contains(t, err.Error(), "pinned is required",
		"buildCall error must explicitly mention that pinned is required")
}

// TestRequeueInterruptMessage_RespectsSessionModelOverride verifies that
// cross-process interrupt requeue successfully resolves and uses a session's
// persisted model override. The bug fixed by P1-4 was that requeueInterruptMessage
// called buildCall with pinned=nil, which fell back to the shared/global model.
//
// This test doesn't directly inspect the queued call (it's in the agent's queue),
// but it verifies the entire call path succeeds: session with override →
// requeueInterruptMessage → resolveSessionModels → buildCall. If the fix were
// not in place, buildCall would reject nil pinned and this test would fail.
func TestRequeueInterruptMessage_RespectsSessionModelOverride(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	// Set up two providers/models.
	const overrideProviderID = "override-provider"
	const sharedProviderID = "shared-provider"

	cfg.Config().Providers.Set(overrideProviderID, config.ProviderConfig{
		ID:   overrideProviderID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "override-large-model", Name: "Override Large"},
			{ID: "override-small-model", Name: "Override Small"},
		},
	})
	cfg.Config().Providers.Set(sharedProviderID, config.ProviderConfig{
		ID:   sharedProviderID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "shared-large-model", Name: "Shared Large"},
			{ID: "shared-small-model", Name: "Shared Small"},
		},
	})

	// Global config uses the shared model (this simulates the process-wide default).
	cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: sharedProviderID,
		Model:    "shared-large-model",
	}
	cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider: sharedProviderID,
		Model:    "shared-small-model",
	}

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	coord.currentAgent = newMockAgent(sharedProviderID, 4096, nil)

	// Create a session with a persisted override that differs from the global default.
	sess, err := env.sessions.Create(t.Context(), "override-session")
	require.NoError(t, err)

	// Apply the override: the session should use overrideProviderID, not sharedProviderID.
	err = env.sessions.UpdateModels(t.Context(), sess.ID, overrideProviderID, "override-large-model", overrideProviderID, "override-small-model")
	require.NoError(t, err)

	// Create a message that will be requeued via interrupt inject.
	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test message"}},
	})
	require.NoError(t, err)

	// Simulate a cross-process interrupt inject by calling requeueInterruptMessage.
	// mockSessionAgent.InterruptAndReplace always reports an owner and records
	// the call it was handed, so the built call is recorded in
	// interruptAndReplaced instead of going through startDetachedRun.
	mock := coord.currentAgent.(*mockSessionAgent)
	err = coord.requeueInterruptMessage(t.Context(), sess.ID, msg.ID, "inject-123")
	require.NoError(t, err, "requeueInterruptMessage must successfully resolve session models")

	// The actual call substance is the point of this regression test: before
	// the fix, buildCall(..., nil, nil) fell back to c.currentAgent.Model()
	// (sharedProviderID), silently discarding the session's override. Assert
	// on the call that was actually built and handed to InterruptAndReplace,
	// not on a second, independent resolveSessionModels call that would pass
	// even if requeueInterruptMessage itself never resolved anything.
	require.Len(t, mock.interruptAndReplaced, 1, "requeueInterruptMessage must hand exactly one call to InterruptAndReplace")
	builtCall := mock.interruptAndReplaced[0]
	require.NotNil(t, builtCall.LargeModel, "built call must carry a resolved large model")
	require.Equal(t, overrideProviderID, builtCall.LargeModel.ModelCfg.Provider,
		"requeued call must use the session's persisted override provider, not the shared/global one")
	require.Equal(t, "override-large-model", builtCall.LargeModel.ModelCfg.Model,
		"requeued call must use the session's persisted override model")
}
