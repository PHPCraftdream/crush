package agent

// Regression tests for task #341/P1-1: a config snapshot pinned at the start
// of a resolve/build operation must stay pinned through every read that
// operation makes, including reads made by helpers it calls into
// (workerSubAgentActive) and reads made later in the same logical operation
// (the 401 credential-refresh rebuild in runInternal).
//
// This is a direct follow-on to the P1-3 fix that made buildAgentModels take
// a single Snapshot() for its own three model lookups (large/small/worker):
// that fix missed that buildAgentModels' worker-preference branch calls
// workerSubAgentActive(), which independently re-read c.cfg.Config() live —
// so a reload landing between buildAgentModels' Snapshot() and the
// workerSubAgentActive() call could still hand back a worker slot from a
// DIFFERENT generation than the large/small models were built from.
// resolveSessionModels had the same shape of gap around its system-prompt
// build, and runInternal's 401 rebuildCall took a SECOND, separately-timed
// Snapshot() purely to look up provider config/options for a model that
// had already been resolved from an earlier snapshot.

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
)

// TestWorkerSubAgentActive_UsesPinnedGeneration proves that
// workerSubAgentActive(cfg) answers strictly from the *config.Config it was
// given, never re-reading c.cfg.Config() live — the exact gap that let
// buildAgentModels' Snapshot() and its own worker-preference decision
// disagree about which generation was in effect.
//
// REVERT CHECK: change the body of workerSubAgentActive back to
//
//	workerModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeWorker]
//
// (ignoring the cfg parameter) and this test fails: after the live config is
// mutated to add a Worker model, the OLD pinned snapshot (which never had a
// Worker slot) would incorrectly start reporting true, because the "live"
// read observes the mutation instead of the frozen snapshot. Restore the
// fix (read from the cfg parameter) and it passes again.
func TestWorkerSubAgentActive_UsesPinnedGeneration(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false) // no Worker configured initially

	// Register the provider the Worker model will point at up front (Config()
	// map mutation here is fine — it happens BEFORE the snapshot this test
	// pins below, so it's part of generation 0, not a later mutation).
	coord.cfg.Config().Providers.Set("worker-provider", config.ProviderConfig{
		ID:   "worker-provider",
		Type: openai.Name,
		Models: []catwalk.Model{
			{ID: "worker-model"},
		},
	})

	// Snapshot BEFORE any worker model is selected.
	pinnedCfg, pinnedGen := coord.cfg.Snapshot()
	require.False(t, coord.workerSubAgentActive(pinnedCfg),
		"no Worker model configured at snapshot time: must report false")

	// Publish a NEW generation that selects the Worker model — via the same
	// copy-on-write path a real reload/runtime config change uses
	// (SetSelectedModelRuntime), not a direct map write. A direct write to
	// coord.cfg.Config().Models would mutate the SAME *Config pointer
	// pinnedCfg already holds (Config()/Snapshot() never clone), which would
	// make this test vacuous — the whole point is that a NEW, independent
	// snapshot gets published, exactly like a real reload landing mid-call.
	coord.cfg.SetSelectedModelRuntime(config.SelectedModelTypeWorker, config.SelectedModel{
		Provider: "worker-provider",
		Model:    "worker-model",
	})

	liveCfg, liveGen := coord.cfg.Snapshot()
	require.Greater(t, liveGen, pinnedGen, "SetSelectedModelRuntime must publish a new generation")

	// The STALE, already-pinned snapshot must still answer as if no Worker
	// exists — it must never observe the later mutation.
	require.False(t, coord.workerSubAgentActive(pinnedCfg),
		"a previously pinned snapshot must not observe a later config mutation")

	// A FRESH snapshot taken after the mutation correctly sees the Worker
	// model — proving the predicate is generation-sensitive, not just always
	// false/broken.
	require.True(t, coord.workerSubAgentActive(liveCfg),
		"a fresh snapshot taken after the mutation must see the new Worker model")
}

// TestBuildAgentModels_ConcurrentReloadNeverTearsWorkerDecision proves seam
// #1 from task #341/P1-1 end to end under real concurrency: buildAgentModels
// takes one Snapshot() and must decide whether to swap in the Worker slot
// from THAT SAME snapshot (via workerSubAgentActive(cfg)), never a live
// re-read. A torn read would show up as a result that is a Frankenstein mix
// of two generations — e.g. ModelCfg.Provider/Model naming the Worker
// provider (decided from a later generation) while the client the model was
// actually built with (large.Model, resolved via largeProviderCfg.Models
// from an earlier or later generation than ModelCfg) came from a different
// one, or vice versa. With a single pinned snapshot per call, every field of
// the returned Model must consistently belong to exactly one of the two
// known-good shapes below, no matter how a concurrent
// SetSelectedModelRuntime interleaves.
//
// REVERT CHECK: pass no argument to workerSubAgentActive (restore
// `c.workerSubAgentActive()`) at the buildAgentModels call site in
// coordinator.go. That reintroduces a live re-read between buildAgentModels'
// own Snapshot() and the worker decision, so under this test's concurrent
// toggling some fraction of calls observe the Snapshot()-time generation for
// large/small model selection but a DIFFERENT (later) generation for the
// worker gate — producing a large.ModelCfg.Provider that doesn't match
// either registered provider's expected model ID pairing, which this test's
// per-call shape assertion catches as a failure. With the fix restored,
// every call is internally consistent by construction (one Snapshot, one
// cfg used throughout), so the race can never manifest here — this is a
// structural guarantee, not a timing-dependent probabilistic catch, which is
// why a modest iteration count run under -race is sufficient.
func TestBuildAgentModels_ConcurrentReloadNeverTearsWorkerDecision(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, true) // Worker configured

	stop := make(chan struct{})
	toggleDone := make(chan struct{})
	go func() {
		defer close(toggleDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				// Worker configured and selectable.
				coord.cfg.SetSelectedModelRuntime(config.SelectedModelTypeWorker, config.SelectedModel{
					Provider: "worker-provider",
					Model:    "worker-model",
				})
			} else {
				// Worker UNSET (zero value) — workerSubAgentActive must
				// report false for a snapshot pinned to THIS generation.
				// This is the state that makes a torn read observable: if
				// the worker-active decision is made from a DIFFERENT
				// (live) generation than the cfg.Models[Worker] value
				// buildAgentModels' worker-swap branch actually assigns
				// from (its OWN pinned cfg), the two can disagree — e.g.
				// the live decision says "active" while the pinned
				// cfg.Models[Worker] this generation actually published is
				// the zero value, producing an empty Provider/Model that
				// buildModelsFromCfg rejects.
				coord.cfg.SetSelectedModelRuntime(config.SelectedModelTypeWorker, config.SelectedModel{})
			}
			i++
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-toggleDone
	})

	for i := 0; i < 500; i++ {
		large, small, err := coord.buildAgentModels(t.Context(), true)
		if err != nil {
			// The only legitimate error here is "worker slot unset in THIS
			// call's own pinned generation, so buildAgentModels correctly
			// did NOT swap to it and instead used Large" -- that path
			// never errors. Any error at all means buildAgentModels'
			// worker branch tried to build from a mismatched/empty
			// selection, i.e. exactly the torn read this test exists to
			// catch.
			t.Fatalf("buildAgentModels returned an error, indicating a torn cross-generation read: %v", err)
		}

		// Whichever generation this call's single Snapshot() landed on, the
		// worker slot at that moment was ALWAYS either fully configured
		// (worker-provider/worker-model) or fully empty (falls back to
		// Large) -- so large.ModelCfg must be exactly one of those two
		// pairs, never an empty Provider with a non-empty Model or vice
		// versa, which is what a torn read between the decision and the
		// value would produce.
		switch large.ModelCfg.Provider {
		case "worker-provider":
			require.Equal(t, "worker-model", large.ModelCfg.Model)
		case "large-provider":
			require.Equal(t, "large-model", large.ModelCfg.Model)
		default:
			t.Fatalf("buildAgentModels returned an impossible provider/model combination: %+v (torn generation read)", large.ModelCfg)
		}
		require.Equal(t, "small-provider", small.ModelCfg.Provider, "small slot must never be affected by the worker decision")
		require.Equal(t, "small-model", small.ModelCfg.Model)
	}
}

// TestResolveSessionModels_ProviderCfgMatchesModelGeneration is the
// regression test for seam #3 (the 401 rebuildCall path): resolveSessionModels
// must return a resolvedOverrides whose providerCfg field was resolved from
// the EXACT SAME snapshot as the large model it also returns. Before this
// fix, runInternal's rebuildCall took its own, separately-timed
// c.cfg.Snapshot() purely to look up provider options for the model
// resolveSessionModels had already resolved — a credential refresh
// (SetProviderRuntimeConfig, which is exactly what 401 handling calls)
// landing in that gap would mix a model built from generation N with
// provider options/credentials from generation N+1.
//
// REVERT CHECK: in resolveSessionModels, delete the
// `resolved.providerCfg = largeProviderCfg` line, and in runInternal's
// rebuildCall closure, replace `providerCfg := pinned.providerCfg` with a
// fresh `snapshotCfg, _ := c.cfg.Snapshot(); providerCfg, _ :=
// snapshotCfg.Providers.Get(model.ModelCfg.Provider)`. This test fails
// because resolved.providerCfg is then the zero value (APIKey == "") even
// though a real provider was configured — proving the field is unpopulated
// without the fix. With the fix restored, resolved.providerCfg carries the
// exact same generation's credentials that produced resolved.large.
func TestResolveSessionModels_ProviderCfgMatchesModelGeneration(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	sess, err := env.sessions.Create(t.Context(), "p1-1 provider-cfg pin probe")
	require.NoError(t, err)

	// First resolve: captures generation N's provider config (API key "key-gen-0").
	coord.cfg.Config().Providers.Set("large-provider", config.ProviderConfig{
		ID:     "large-provider",
		Type:   openai.Name,
		APIKey: "key-gen-0",
		Models: []catwalk.Model{{ID: "large-model"}},
	})
	resolvedGen0, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "large-provider", resolvedGen0.large.ModelCfg.Provider)
	require.Equal(t, "key-gen-0", resolvedGen0.providerCfg.APIKey,
		"providerCfg must be populated from the SAME snapshot that built resolved.large")

	// Simulate a credential refresh landing after resolveSessionModels
	// returned generation N's snapshot — SetProviderRuntimeConfig is exactly
	// what the coordinator's 401 handling calls to apply a freshly resolved
	// API key/token in-memory, and it bumps the generation.
	genBefore := coord.cfg.Generation()
	coord.cfg.SetProviderRuntimeConfig("large-provider", config.ProviderConfig{
		ID:     "large-provider",
		Type:   openai.Name,
		APIKey: "key-gen-1",
		Models: []catwalk.Model{{ID: "large-model"}},
	})
	require.Greater(t, coord.cfg.Generation(), genBefore, "SetProviderRuntimeConfig must publish a new generation")

	// resolvedGen0, already returned, must NOT observe the credential
	// refresh: it is an immutable snapshot of generation N. This is exactly
	// what runInternal's `run()` closure (using the ORIGINAL pinned call,
	// built before any 401 was even detected) relies on.
	require.Equal(t, "key-gen-0", resolvedGen0.providerCfg.APIKey,
		"a previously resolved snapshot must not observe a later credential refresh")

	// A FRESH resolveSessionModels call (what rebuildCall does after a 401)
	// must see the refreshed credential, proving the pin is generation-
	// sensitive rather than permanently stale.
	resolvedGen1, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "key-gen-1", resolvedGen1.providerCfg.APIKey,
		"a fresh resolve after credential refresh must pick up the new generation's provider config")

	// And within THAT fresh resolve, large and providerCfg must still agree
	// with each other (both generation N+1) — the core seam #3 property.
	require.Equal(t, "large-provider", resolvedGen1.large.ModelCfg.Provider)
	require.Equal(t, resolvedGen1.large.ModelCfg.Provider, resolvedGen1.providerCfg.ID,
		"resolved.large and resolved.providerCfg must describe the same provider from the same resolve call")
}
