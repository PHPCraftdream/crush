package agent

// Regression tests for the per-session model isolation fix (user-reported:
// switching the model in the web UI changed it globally instead of per
// session) and for a correctness bug an independent review found in the
// first version of that fix's model cache.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveSessionModels_SmallModelNotSwappedWithLarge is the regression
// test for a bug found reviewing the /crush-delegated per-session model
// isolation fix: resolveSessionModels's original model cache called
// buildModelsFromCfg once per slot, swapping (largeCfg, smallCfg) argument
// order to "select" which half of the pair to keep for the small slot.
// buildModelsFromCfg(ctx, smallCfg, largeCfg, false) returns
// (ModelBuiltFromSmallCfg, ModelBuiltFromLargeCfg) — its second return
// value is built from largeCfg regardless of which local variable name it
// gets assigned to. The buggy code picked that second value as the small
// model's result, so resolved.small silently held a Model built from the
// LARGE config's provider/model whenever large and small configs differed
// — pinned onto every turn's SmallModel (title generation and any other
// small-model-driven path) via resolvedOverrides.pin.
//
// This bug was invisible to the /crush sub-agent's own testing because its
// test fixtures likely configure identical large/small models (a common CI
// shortcut) — the swap's condition (cfg == largeCfg) is then trivially
// true either way, masking the mismatch. This test deliberately uses
// DISTINCT large/small providers, matching newWorkerToolTestCoordinator's
// registerProvider setup already used elsewhere in this test file.
//
// REVERT CHECK: reintroduce the per-slot buildModel closure with swapped
// buildModelsFromCfg args (as landed by the delegated fix before this
// review caught it), this test fails with resolved.small.ModelCfg.Model
// == "large-model" instead of "small-model"; restore the single
// buildModelsFromCfg(ctx, largeCfg, smallCfg, false) call cached as a
// pair, it passes again.
func TestResolveSessionModels_SmallModelNotSwappedWithLarge(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	sess, err := env.sessions.Create(t.Context(), "model isolation probe")
	require.NoError(t, err)

	resolved, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	require.Equal(t, "large-provider", resolved.large.ModelCfg.Provider)
	require.Equal(t, "large-model", resolved.large.ModelCfg.Model)
	require.Equal(t, "small-provider", resolved.small.ModelCfg.Provider,
		"small model's provider must come from the small config, not silently swapped for the large one")
	require.Equal(t, "small-model", resolved.small.ModelCfg.Model,
		"small model's model ID must come from the small config, not silently swapped for the large one")
}

// TestResolveSessionModels_CachesPairAcrossCalls proves the model cache
// added by the isolation fix actually gets used on a second resolve for
// the same (large, small) config pair — the cache exists specifically to
// avoid rebuilding a provider client on every turn (previously amortized
// for free by mutating shared state; the isolation fix's cache is the
// explicit replacement for that).
func TestResolveSessionModels_CachesPairAcrossCalls(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	sessA, err := env.sessions.Create(t.Context(), "cache probe A")
	require.NoError(t, err)
	sessB, err := env.sessions.Create(t.Context(), "cache probe B")
	require.NoError(t, err)

	// Neither session has a DB override, so both resolve to the same
	// config-default (large-provider/large-model, small-provider/small-model)
	// pair and should hit the same cache entry.
	resolvedA, err := coord.resolveSessionModels(t.Context(), sessA.ID)
	require.NoError(t, err)
	resolvedB, err := coord.resolveSessionModels(t.Context(), sessB.ID)
	require.NoError(t, err)

	require.Equal(t, resolvedA.large.ModelCfg, resolvedB.large.ModelCfg)
	require.Equal(t, resolvedA.small.ModelCfg, resolvedB.small.ModelCfg)
	require.Equal(t, 1, coord.modelCache.Len(), "both sessions share one config-default pair, so exactly one cache entry must exist")
}
