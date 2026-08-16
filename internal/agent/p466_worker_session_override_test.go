package agent

// Regression tests for task #466: a session-level worker-slot model override
// (set via the new set_session_models-equivalent worker/reviewer plumbing,
// internal/session's UpdateWorkerReviewerModels) must actually change which
// model a sub-agent dispatch from THAT session uses, without leaking into a
// concurrent sub-agent dispatch from a different session sharing the same
// coordinator-wide "task" agent object.
//
// See resolveSubAgentModelOverride's doc comment (internal/agent/coordinator.go)
// for why this is a separate, lighter path than resolveSessionModels, and why
// reviewer has no equivalent runtime hook.

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestResolveSubAgentModelOverride_NoSessionOverrideReturnsNil proves the
// cheap-fallback path: a session that never set a worker override gets nil
// back, so runSubAgent keeps using the coordinator-wide default agent's own
// model instead of paying to rebuild an identical one.
func TestResolveSubAgentModelOverride_NoSessionOverrideReturnsNil(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, true)

	sess, err := env.sessions.Create(t.Context(), "no worker override")
	require.NoError(t, err)

	override, err := coord.resolveSubAgentModelOverride(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Nil(t, override)
}

// TestResolveSubAgentModelOverride_UsesSessionWorkerOverride is the direct
// regression test: a session with an explicit worker override resolves to
// THAT model, not the coordinator-wide worker-provider/worker-model default.
func TestResolveSubAgentModelOverride_UsesSessionWorkerOverride(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, true)

	// A provider distinct from the coordinator-wide "worker-provider"
	// default, so a passing test can only mean the session override was
	// actually read, not that the default happened to match.
	coord.cfg.Config().Providers.Set("session-worker-provider", config.ProviderConfig{
		ID:   "session-worker-provider",
		Type: catwalk.TypeOpenAI,
		Models: []catwalk.Model{
			{ID: "session-worker-model"},
		},
	})

	sess, err := env.sessions.Create(t.Context(), "with worker override")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateWorkerReviewerModels(t.Context(), sess.ID,
		&session.ModelSlotUpdate{Provider: "session-worker-provider", Model: "session-worker-model"}, nil))

	override, err := coord.resolveSubAgentModelOverride(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, override)
	require.Equal(t, "session-worker-provider", override.ModelCfg.Provider)
	require.Equal(t, "session-worker-model", override.ModelCfg.Model)
}

// TestResolveSubAgentModelOverride_IsolatedAcrossSessions is the isolation
// guarantee: two sessions with DIFFERENT worker overrides must each resolve
// to their OWN override, proving the per-call SessionAgentCall.LargeModel
// pin (not a shared, mutable agent field) is what carries the value —
// exactly the class of cross-session leak task #341/P0-1 fixed for
// large/small, now guarded for worker too.
func TestResolveSubAgentModelOverride_IsolatedAcrossSessions(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, true)

	registerProvider := func(id, model string) {
		coord.cfg.Config().Providers.Set(id, config.ProviderConfig{
			ID:   id,
			Type: catwalk.TypeOpenAI,
			Models: []catwalk.Model{
				{ID: model},
			},
		})
	}
	registerProvider("worker-provider-a", "worker-model-a")
	registerProvider("worker-provider-b", "worker-model-b")

	sessA, err := env.sessions.Create(t.Context(), "worker override A")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateWorkerReviewerModels(t.Context(), sessA.ID,
		&session.ModelSlotUpdate{Provider: "worker-provider-a", Model: "worker-model-a"}, nil))

	sessB, err := env.sessions.Create(t.Context(), "worker override B")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateWorkerReviewerModels(t.Context(), sessB.ID,
		&session.ModelSlotUpdate{Provider: "worker-provider-b", Model: "worker-model-b"}, nil))

	overrideA, err := coord.resolveSubAgentModelOverride(t.Context(), sessA.ID)
	require.NoError(t, err)
	overrideB, err := coord.resolveSubAgentModelOverride(t.Context(), sessB.ID)
	require.NoError(t, err)

	require.Equal(t, "worker-provider-a", overrideA.ModelCfg.Provider)
	require.Equal(t, "worker-provider-b", overrideB.ModelCfg.Provider,
		"session B's override must not have been overwritten by session A's resolution")
}
