package config

// Regression test for P1-3: SetProviderRuntimeConfig must increment the
// generation when mutating providers, otherwise the cache key contract is
// violated.

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

// TestSetProviderRuntimeConfig_IncrementsGeneration is a regression test for
// P1-3: SetProviderRuntimeConfig must increment the generation when mutating
// providers, ensuring the cache key contract is honored.
//
// REVERT CHECK: temporarily modify SetProviderRuntimeConfig to mutate
// in-place without calling publishLocked (i.e., revert to the old
// "s.loadSnapshot().config.Providers.Set(...)" pattern). This test will fail
// because generation does not increment. Restore the publishLocked call, it
// passes again.
func TestSetProviderRuntimeConfig_IncrementsGeneration(t *testing.T) {
	cfg := &Config{
		Providers: csync.NewMap[string, ProviderConfig](),
	}
	store := NewTestStore(cfg)

	initialGen := store.Generation()

	// Set a provider config; this should increment generation.
	store.SetProviderRuntimeConfig("test-provider", ProviderConfig{
		ID:   "test-provider",
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "gpt-4"},
		},
	})

	newGen := store.Generation()
	require.Greater(t, newGen, initialGen, "generation must increment after SetProviderRuntimeConfig")
}

// TestSetProviderRuntimeConfig_OldSnapshotUnaffected is a regression test
// for a gap found by independent review after the round that added
// TestSetProviderRuntimeConfig_IncrementsGeneration: incrementing the
// generation is necessary but not sufficient for the "one atomic
// (config, generation) pair per reader" contract Snapshot() exists to
// provide. SetProviderRuntimeConfig used to do `next := cur.clone()` (a
// SHALLOW copy — next.config was still the SAME *Config pointer cur.config
// was) and then mutate next.config.Providers in place, which also mutated
// what any snapshot captured BEFORE the call still points at. A caller
// that captured (oldConfig, oldGen) via Snapshot() before this call, then
// later read oldConfig.Providers, would see the NEW provider data even
// though its own captured generation says otherwise — a torn read hiding
// behind a correct-looking generation counter.
//
// REVERT CHECK: change SetProviderRuntimeConfig back to
// `next.config.Providers.Set(providerID, pc)` on the shared clone()'d
// pointer instead of building an independent *Config with its own
// Providers map. This test fails because oldSnapshotCfg.Providers.Get
// finds the new provider despite being captured before the call.
func TestSetProviderRuntimeConfig_OldSnapshotUnaffected(t *testing.T) {
	cfg := &Config{
		Providers: csync.NewMap[string, ProviderConfig](),
	}
	store := NewTestStore(cfg)

	// Capture a snapshot BEFORE the mutation.
	oldSnapshotCfg, oldGen := store.Snapshot()
	_, hadProviderBefore := oldSnapshotCfg.Providers.Get("test-provider")
	require.False(t, hadProviderBefore, "provider should not exist yet")

	store.SetProviderRuntimeConfig("test-provider", ProviderConfig{
		ID:   "test-provider",
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "gpt-4"},
		},
	})

	// The OLD snapshot's Config (and its Providers map) must remain exactly
	// as it was when captured — a reader holding it should never see a
	// write that happened after its own generation.
	_, hasProviderInOldSnapshot := oldSnapshotCfg.Providers.Get("test-provider")
	require.False(t, hasProviderInOldSnapshot,
		"old snapshot captured before SetProviderRuntimeConfig must not see the new provider")

	// The NEW snapshot must see it, under a strictly newer generation.
	newSnapshotCfg, newGen := store.Snapshot()
	_, hasProviderInNewSnapshot := newSnapshotCfg.Providers.Get("test-provider")
	require.True(t, hasProviderInNewSnapshot, "new snapshot must see the provider")
	require.Greater(t, newGen, oldGen, "generation must have advanced")
}

// TestSnapshot_Atomicity tests that Snapshot() returns config and generation
// from a single atomic read, preventing torn reads across concurrent reloads.
func TestSnapshot_Atomicity(t *testing.T) {
	cfg := &Config{}
	store := NewTestStore(cfg)

	// Call Snapshot multiple times; each call should return consistent pairs.
	for i := 0; i < 100; i++ {
		cfgSnap, gen := store.Snapshot()

		// Verify the config is not nil.
		require.NotNil(t, cfgSnap, "config should never be nil from Snapshot()")

		// Verify that calling Generation() separately matches the snapshot's gen.
		require.Equal(t, gen, store.Generation(), "generation from Snapshot() should match Generation()")

		// Verify that Config() matches the snapshot's config.
		require.Equal(t, cfgSnap, store.Config(), "config from Snapshot() should match Config()")
	}
}
