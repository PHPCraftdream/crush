package agent

// Task #464 — locks the full system -> folder -> session cascade at the
// point resolveSessionModels actually reads it. internal/config's
// p464_cascade_test.go locks the system -> folder half (the merged
// cfg.Models[...] resolveSessionModels starts from); this file proves the
// session DB override correctly layers ON TOP of an ALREADY folder-shadowed
// config, and that clearing it falls back to that same merged value — not
// to the system default directly, which would be wrong if folder and
// system disagree.

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFullCascade_SessionOverridesAlreadyFolderShadowedConfig(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	// newWorkerToolTestCoordinator's registerProvider sets Models[Large]
	// directly on the in-memory cfg (no real global/workspace scope
	// layering involved here — that's covered in internal/config). Treat
	// this as "the merged, already-effective config" and register a THIRD,
	// distinct provider to stand in for the session's own override, so a
	// passing test can only mean the session layer was actually consulted.
	coord.cfg.Config().Providers.Set("session-provider", config.ProviderConfig{
		ID:   "session-provider",
		Type: catwalk.TypeOpenAI,
		Models: []catwalk.Model{
			{ID: "session-model"},
		},
	})

	sess, err := env.sessions.Create(t.Context(), "full cascade probe")
	require.NoError(t, err)

	// Before any session override: resolves to the coordinator-wide
	// (stand-in for folder-shadowed-system) default.
	before, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "large-provider", before.large.ModelCfg.Provider)

	// Session sets its own override — must win over the merged config.
	require.NoError(t, env.sessions.UpdateModels(t.Context(), sess.ID,
		&session.ModelSlotUpdate{Provider: "session-provider", Model: "session-model"}, nil))

	after, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "session-provider", after.large.ModelCfg.Provider, "session override must win over the merged system/folder config")
	require.Equal(t, "session-model", after.large.ModelCfg.Model)

	// Clearing the session override (explicit empty, not nil — task #467's
	// "Inherit") must fall back to the SAME merged value as `before`, not
	// to some other default.
	require.NoError(t, env.sessions.UpdateModels(t.Context(), sess.ID,
		&session.ModelSlotUpdate{Provider: "", Model: ""}, nil))

	restored, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, before.large.ModelCfg.Provider, restored.large.ModelCfg.Provider,
		"clearing the session override must restore exactly the merged system/folder value")
	require.Equal(t, before.large.ModelCfg.Model, restored.large.ModelCfg.Model)
}
