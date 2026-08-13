package config

// Regression test for P1-2: RemoveProviderAPIKey must publish a new
// snapshot (with an incremented generation and an independent Providers
// map) instead of mutating the already-published snapshot's Providers map
// in place.

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

// TestRemoveProviderAPIKey_OldSnapshotUnaffected is a regression test for
// P1-2: RemoveProviderAPIKey used to do
// `s.loadSnapshot().config.Providers.Del(providerID)` directly after
// RemoveConfigField's own reload had already published a new snapshot. That
// Del call mutated the *csync.Map belonging to the just-published snapshot
// in place, with no generation bump of its own and no cache invalidation,
// and also retroactively changed what any snapshot captured BEFORE the Del
// call still sees for providerID — a torn read hiding behind a
// correct-looking generation counter, same class of bug fixed for
// SetProviderRuntimeConfig in task #341 (P1-3).
//
// REVERT CHECK: temporarily change RemoveProviderAPIKey back to
// `s.loadSnapshot().config.Providers.Del(providerID)` (no publishLocked,
// operating on the shared, already-published *Config). This test fails
// because oldSnapshotCfg.Providers.Get no longer finds the provider despite
// being captured before the call (i.e. the old snapshot's view changes
// retroactively). Restore the publishLocked-based fix and it passes again.
func TestRemoveProviderAPIKey_OldSnapshotUnaffected(t *testing.T) {
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("test-provider", ProviderConfig{
		ID:     "test-provider",
		Type:   "openai",
		APIKey: "sk-test",
	})
	cfg := &Config{
		Providers: providers,
	}
	store := newTestConfigStore(testStoreOpts{
		config:         cfg,
		globalDataPath: filepath.Join(t.TempDir(), "crush.json"),
	})

	// Capture a snapshot BEFORE the removal.
	oldSnapshotCfg, oldGen := store.Snapshot()
	_, hadProviderBefore := oldSnapshotCfg.Providers.Get("test-provider")
	require.True(t, hadProviderBefore, "provider should exist before removal")

	err := store.RemoveProviderAPIKey(ScopeGlobal, "test-provider")
	require.NoError(t, err)

	// The OLD snapshot's Config (and its Providers map) must remain exactly
	// as it was when captured — a reader holding it should never see a
	// write that happened after its own generation.
	_, hasProviderInOldSnapshot := oldSnapshotCfg.Providers.Get("test-provider")
	require.True(t, hasProviderInOldSnapshot,
		"old snapshot captured before RemoveProviderAPIKey must still see the provider")

	// The NEW snapshot must no longer see it, under a strictly newer
	// generation.
	newSnapshotCfg, newGen := store.Snapshot()
	_, hasProviderInNewSnapshot := newSnapshotCfg.Providers.Get("test-provider")
	require.False(t, hasProviderInNewSnapshot, "new snapshot must not see the removed provider")
	require.Greater(t, newGen, oldGen, "generation must have advanced")
}

// TestRemoveProviderAPIKey_IncrementsGeneration is a regression test for
// P1-2: RemoveProviderAPIKey must increment the generation when removing a
// provider, ensuring the cache key contract is honored the same way
// SetProviderRuntimeConfig (P1-3) does for provider updates.
func TestRemoveProviderAPIKey_IncrementsGeneration(t *testing.T) {
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("test-provider", ProviderConfig{
		ID:     "test-provider",
		Type:   "openai",
		APIKey: "sk-test",
	})
	cfg := &Config{
		Providers: providers,
	}
	store := newTestConfigStore(testStoreOpts{
		config:         cfg,
		globalDataPath: filepath.Join(t.TempDir(), "crush.json"),
	})

	initialGen := store.Generation()

	err := store.RemoveProviderAPIKey(ScopeGlobal, "test-provider")
	require.NoError(t, err)

	newGen := store.Generation()
	require.Greater(t, newGen, initialGen, "generation must increment after RemoveProviderAPIKey")
}
