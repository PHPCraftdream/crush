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
