package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
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
