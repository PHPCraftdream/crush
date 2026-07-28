package config

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// writeGenerationConfig writes a crush.json whose single custom provider's
// model ID embeds gen, so that after a reload picks it up,
// Config().Models[large].Model, Config().Providers, and KnownProviders()
// (via the custom-provider validation path, which mirrors provider IDs seen
// in Providers) all reflect the SAME generation number. This lets a reader
// detect a "torn" snapshot: if it ever observes a config whose model name
// says generation N but reads some other piece of the snapshot describing
// a different generation, the fields were published as more than one
// atomic unit — exactly what storeSnapshot is meant to prevent.
func writeGenerationConfig(t *testing.T, path string, gen int) {
	t.Helper()
	modelID := fmt.Sprintf("model-gen-%d", gen)
	content := fmt.Sprintf(`{
		"providers": {
			"genprovider": {
				"api_key": "test-key",
				"base_url": "https://example.invalid/v1",
				"models": [{"id": %q, "name": %q}]
			}
		},
		"models": {
			"large": {"provider": "genprovider", "model": %q},
			"small": {"provider": "genprovider", "model": %q}
		}
	}`, modelID, modelID, modelID, modelID)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// TestConcurrentReloadAndReads_NeverObservesTornGeneration hammers
// ReloadFromDisk from several goroutines while other goroutines
// concurrently call Config()/Resolver()/KnownProviders() in a tight loop.
// Under `go test -race` this must report no data race, and every read must
// see internally-consistent state: a snapshot's Config().Models[large]
// generation marker must match the provider config's generation marker
// from the SAME storeSnapshot, never a mix of an old config with a newer
// (or older) provider set — which is exactly what could happen if reload
// still assigned s.config, s.resolver, s.knownProviders etc. as separate,
// non-atomic field writes.
func TestConcurrentReloadAndReads_NeverObservesTornGeneration(t *testing.T) {
	dir := t.TempDir()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", dir)
	t.Setenv("CRUSH_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)

	configPath := filepath.Join(dir, "crush.json")
	writeGenerationConfig(t, configPath, 0)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Kept deliberately small: reloadFromDiskLocked does real disk I/O,
	// JSON marshal/unmarshal, and provider (re)configuration on every
	// call, and this whole test runs under -race instrumentation. The
	// goal is proving the atomic-publish invariant holds under genuine
	// concurrency, not stress-testing throughput — a handful of
	// interleaved reloads racing a pool of readers is already enough to
	// hit the old bug reliably (the original, unfixed code tore on
	// essentially every reload).
	const readers = 4
	const reloaders = 2
	const generations = 5

	var stop atomic.Bool
	var tornGenerations atomic.Int64
	var readCount atomic.Int64

	var readersWg sync.WaitGroup
	var reloadersWg sync.WaitGroup

	// pairings records, for every distinct *Config pointer any reader has
	// ever observed, which Resolver (by pointer identity) and which
	// KnownProviders slice header (by address of its first element) were
	// paired with it. reloadFromDiskLocked always builds a brand new
	// resolver (NewShellVariableResolver) and a brand new knownProviders
	// slice on every single reload, together with the new *Config — so
	// under a correctly atomic publish, one *Config pointer can NEVER be
	// seen paired with two different Resolver/KnownProviders values across
	// reads: config/resolver/knownProviders only ever change together, in
	// the same storeSnapshot. If a reader ever observes the SAME config
	// pointer paired with a DIFFERENT resolver or knownProviders than a
	// previous read recorded for that same config pointer, the store
	// published (or a reader observed) a torn generation — exactly the
	// original bug's symptom (s.config, s.resolver, s.knownProviders as
	// three independently-assigned fields). This check is deliberately
	// cross-field (Config() vs Resolver() vs KnownProviders()), unlike a
	// same-cfg internal check (e.g. cfg.Models vs cfg.Providers), which
	// can't detect a tear between separate top-level snapshot fields since
	// both would come from the same *Config either way.
	var pairingsMu sync.Mutex
	resolverForConfig := make(map[*Config]VariableResolver)
	knownPtrForConfig := make(map[*Config]*catwalk.Provider)

	// Readers: repeatedly read Config()/Resolver()/KnownProviders() and
	// cross-check the pairing recorded above, plus the within-cfg
	// model/provider consistency check as a secondary signal. A short
	// sleep keeps the readers from starving the reloaders' CPU time
	// (reload itself does the heavy lifting; readers just need to sample
	// frequently, not constantly). Readers run on their own WaitGroup,
	// stopped only after the reloaders finish — waiting on a single shared
	// WaitGroup here would deadlock, since readers only exit once `stop`
	// is set, and `stop` is only set after learning the reloaders are
	// done.
	for range readers {
		readersWg.Add(1)
		go func() {
			defer readersWg.Done()
			for !stop.Load() {
				cfg := store.Config()
				resolver := store.Resolver()
				known := store.KnownProviders()
				readCount.Add(1)

				if cfg != nil && resolver != nil {
					if large, ok := cfg.Models[SelectedModelTypeLarge]; ok {
						if pc, ok := cfg.Providers.Get("genprovider"); ok && len(pc.Models) > 0 {
							// The model selected for "large" must always be
							// exactly the one model this provider config
							// carries — a same-cfg sanity check.
							if large.Model != pc.Models[0].ID {
								tornGenerations.Add(1)
							}
						}
					}

					var knownPtr *catwalk.Provider
					if len(known) > 0 {
						knownPtr = &known[0]
					}

					pairingsMu.Lock()
					if prevResolver, seen := resolverForConfig[cfg]; seen && prevResolver != resolver {
						tornGenerations.Add(1)
					} else {
						resolverForConfig[cfg] = resolver
					}
					if prevKnown, seen := knownPtrForConfig[cfg]; seen && prevKnown != knownPtr {
						tornGenerations.Add(1)
					} else {
						knownPtrForConfig[cfg] = knownPtr
					}
					pairingsMu.Unlock()
				}

				time.Sleep(time.Millisecond)
			}
		}()
	}

	// Reloaders: repeatedly rewrite the config file with a new generation
	// marker and call ReloadFromDisk.
	for i := range reloaders {
		reloadersWg.Add(1)
		go func(id int) {
			defer reloadersWg.Done()
			for g := 1; g <= generations; g++ {
				writeGenerationConfig(t, configPath, id*generations+g)
				_ = store.ReloadFromDisk(context.Background())
			}
		}(i)
	}

	// Wait only for the reloaders; then signal readers to stop and wait
	// for them too, bounded so a genuine hang still fails fast instead of
	// blocking the whole suite.
	reloadersDone := make(chan struct{})
	go func() {
		reloadersWg.Wait()
		close(reloadersDone)
	}()
	select {
	case <-reloadersDone:
	case <-time.After(60 * time.Second):
		t.Fatal("reloaders did not finish within 60s")
	}
	stop.Store(true)

	readersDone := make(chan struct{})
	go func() {
		readersWg.Wait()
		close(readersDone)
	}()
	select {
	case <-readersDone:
	case <-time.After(10 * time.Second):
		t.Fatal("readers did not stop within 10s of the stop signal")
	}

	require.Equal(t, int64(0), tornGenerations.Load(), "reader observed a torn generation: config.Models[large] disagreed with config.Providers for the same snapshot")
	require.Greater(t, readCount.Load(), int64(0), "test sanity: readers should have run at least once")
}

// TestFailedReload_DoesNotChangePublishedSnapshot verifies that a reload
// which fails validation (invalid hook config) does not alter what
// Config() returns at all — not the pointer, not the content. Before this
// refactor, reloadFromDiskLocked assigned s.config = cfg first and only
// rolled the individual fields back to saved locals on error; with
// storeSnapshot + atomic.Pointer, a failed reload simply never calls
// snap.Store, so there is nothing to roll back.
func TestFailedReload_DoesNotChangePublishedSnapshot(t *testing.T) {
	dir := t.TempDir()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", dir)
	t.Setenv("CRUSH_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)

	configPath := filepath.Join(dir, "crush.json")
	writeGenerationConfig(t, configPath, 1)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	before := store.Config()
	beforeModel := before.Models[SelectedModelTypeLarge]
	require.Equal(t, "model-gen-1", beforeModel.Model)

	// Write a config with an invalid hook matcher regex — ValidateHooks
	// must reject this during reload.
	badConfig := `{
		"providers": {
			"genprovider": {
				"api_key": "test-key",
				"base_url": "https://example.invalid/v1",
				"models": [{"id": "model-gen-2", "name": "model-gen-2"}]
			}
		},
		"models": {
			"large": {"provider": "genprovider", "model": "model-gen-2"}
		},
		"hooks": {
			"PreToolUse": [
				{"matcher": "[invalid(regex", "command": "exit 0"}
			]
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(badConfig), 0o600))

	reloadErr := store.ReloadFromDisk(context.Background())
	require.Error(t, reloadErr, "reload with an invalid hook matcher must fail")

	after := store.Config()
	require.Same(t, before, after, "Config() must return the exact same *Config after a failed reload")
	require.Equal(t, "model-gen-1", after.Models[SelectedModelTypeLarge].Model, "failed reload must not change the published model selection")
}

// TestCopyOnWrite_OldReaderUnaffectedByLaterMutation is the regression test
// for the app.go bug pattern: app.config.Config().Models[...] = ... used to
// mutate the map returned by Config() directly, in a different package,
// with no synchronization at all. It demonstrates the fix (copy-on-write
// via updateConfig/SetSelectedModelRuntime) actually isolates readers: a
// goroutine that already captured a *Config via Config() before a mutation
// must keep seeing the OLD value even after the mutation is published,
// because the mutator built a new *Config rather than writing through the
// shared one.
func TestCopyOnWrite_OldReaderUnaffectedByLaterMutation(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "openai", Model: "gpt-4o"},
		},
	}
	store := NewTestStore(cfg)

	// Reader captures the config BEFORE the mutation.
	oldCfg := store.Config()
	oldModel := oldCfg.Models[SelectedModelTypeLarge]
	require.Equal(t, "gpt-4o", oldModel.Model)

	// Mutate via the runtime-only copy-on-write setter (mirrors app.go's
	// overrideModelsForNonInteractive path).
	store.SetSelectedModelRuntime(SelectedModelTypeLarge, SelectedModel{
		Provider: "anthropic",
		Model:    "claude-x",
	})

	// The OLD snapshot's Config, and the map inside it, must be completely
	// unaffected by the mutation that came after it was captured.
	require.Equal(t, "gpt-4o", oldCfg.Models[SelectedModelTypeLarge].Model, "a *Config captured before a copy-on-write mutation must not observe that mutation")

	// A fresh Config() call must see the new value.
	newCfg := store.Config()
	require.Equal(t, "claude-x", newCfg.Models[SelectedModelTypeLarge].Model)

	// And the two *Config pointers must differ — proof the mutation built
	// a new top-level Config rather than writing through the old one.
	require.NotSame(t, oldCfg, newCfg)
}

// TestConcurrentUpdateConfig_DoesNotLoseUpdates fires the shared
// copy-on-write primitive (updateConfig, the same helper UpdatePreferredModels/
// SetCompactMode/SetTheme/etc. all funnel through) from many goroutines,
// each targeting a distinct Models map key so the expected end state is
// fully deterministic (no two goroutines contend for the same key's final
// value), and asserts every single write is still visible at the end.
//
// This deliberately exercises the in-memory copy-on-write path only (not
// the SetConfigFields disk round-trip that UpdatePreferredModels also
// performs) because concurrent writers to the very same on-disk path is a
// separate, pre-existing concern in atomicWriteFile's rename-based publish
// (on Windows, two goroutines racing os.Rename onto the same destination
// can transiently fail with "Access is denied" — orthogonal to the
// snapshot/copy-on-write mechanism this task is about). What this task's
// invariant actually requires is: without writeMu serialising the
// read-clone-mutate-publish cycle, goroutine A could clone the snapshot,
// goroutine B could clone the SAME (still-current) snapshot, A publishes,
// then B publishes its own clone (built from the pre-A state) and silently
// discards A's change — even though A and B touched different map keys.
// writeMu prevents that interleaving. Run with -race to also confirm the
// underlying map accesses are synchronized.
func TestConcurrentUpdateConfig_DoesNotLoseUpdates(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	store := NewTestStore(cfg)

	// Use the four real SelectedModelType slots so we have enough
	// independent keys to fan out across goroutines without any two
	// writers targeting the same key (which would have a legitimately
	// ambiguous "last writer wins" outcome).
	modelTypes := []SelectedModelType{
		SelectedModelTypeLarge,
		SelectedModelTypeSmall,
		SelectedModelTypeWorker,
		SelectedModelTypeReviewer,
	}

	var wg sync.WaitGroup
	for i, mt := range modelTypes {
		wg.Add(1)
		go func(i int, mt SelectedModelType) {
			defer wg.Done()
			model := SelectedModel{Provider: "p", Model: fmt.Sprintf("m-%d", i)}
			store.updateConfig(func(cfgCopy *Config) {
				cfgCopy.Models = maps.Clone(cfgCopy.Models)
				if cfgCopy.Models == nil {
					cfgCopy.Models = make(map[SelectedModelType]SelectedModel)
				}
				cfgCopy.Models[mt] = model
			})
		}(i, mt)
	}
	wg.Wait()

	final := store.Config()
	for i, mt := range modelTypes {
		got, ok := final.Models[mt]
		require.True(t, ok, "model type %s missing from final config — update was lost", mt)
		require.Equal(t, fmt.Sprintf("m-%d", i), got.Model, "model type %s has wrong value — a concurrent write was lost", mt)
	}
}

// TestConcurrentUpdatePreferredModels_SingleWriterSemantics exercises the
// full public UpdatePreferredModels path (in-memory update + disk
// persistence + recent-models bookkeeping) end to end. Disk writes for
// different scopes/paths don't contend, so this uses two independent
// stores (global vs workspace) writing concurrently to prove the on-disk
// + in-memory halves both land correctly when there is no shared-file
// contention — the shared-file, same-path concurrent-write case is a
// separate, pre-existing atomicWriteFile limitation and is intentionally
// not exercised by writing N goroutines at the identical path (see
// TestConcurrentUpdateConfig_DoesNotLoseUpdates for the in-memory-only
// concurrent case, which IS this task's target).
func TestConcurrentUpdatePreferredModels_SingleWriterSemantics(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global.json")
	workspacePath := filepath.Join(dir, "workspace.json")
	require.NoError(t, os.WriteFile(globalPath, []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(workspacePath, []byte(`{}`), 0o600))

	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := newTestConfigStore(testStoreOpts{
		config:         cfg,
		globalDataPath: globalPath,
		workspacePath:  workspacePath,
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := store.UpdatePreferredModels(ScopeGlobal, map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "p", Model: "global-large"},
		})
		require.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		err := store.UpdatePreferredModels(ScopeWorkspace, map[SelectedModelType]SelectedModel{
			SelectedModelTypeSmall: {Provider: "p", Model: "workspace-small"},
		})
		require.NoError(t, err)
	}()
	wg.Wait()

	final := store.Config()
	require.Equal(t, "global-large", final.Models[SelectedModelTypeLarge].Model, "global-scope update was lost")
	require.Equal(t, "workspace-small", final.Models[SelectedModelTypeSmall].Model, "workspace-scope update was lost")
}

// TestConcurrentUpdateConfig_DifferentNestedFields_BothVisible fires two
// concurrent updateConfig writers that mutate DIFFERENT nested fields of
// Options.TUI (CompactMode vs Theme). Without publishMu serialising the
// read-clone-mutate-publish cycle, both writers would clone the SAME
// starting snapshot, each apply only their own field, and the second
// Store() would silently discard the first writer's field — even though
// the two fields are completely independent.
//
// This covers the same class of bug as
// TestConcurrentUpdateConfig_DoesNotLoseUpdates but exercises the
// Options/TUIOptions clone path (cloneOptions + cloneTUIOptions) instead
// of the Models map path — the exact pattern SetCompactMode and SetTheme
// use internally. The start barrier and inner loop widen the race window
// so the test reliably catches the bug if publishMu is ever split back
// into separate writer/reload locks.
func TestConcurrentUpdateConfig_DifferentNestedFields_BothVisible(t *testing.T) {
	t.Parallel()

	const iterations = 20
	for range iterations {
		cfg := &Config{}
		cfg.setDefaults(t.TempDir(), "")
		store := NewTestStore(cfg)

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			store.updateConfig(func(c *Config) {
				optsCopy := cloneOptions(c.Options)
				tuiCopy := cloneTUIOptions(optsCopy.TUI)
				tuiCopy.CompactMode = true
				optsCopy.TUI = tuiCopy
				c.Options = optsCopy
			})
		}()

		go func() {
			defer wg.Done()
			<-start
			store.updateConfig(func(c *Config) {
				optsCopy := cloneOptions(c.Options)
				tuiCopy := cloneTUIOptions(optsCopy.TUI)
				tuiCopy.Theme = "dark"
				optsCopy.TUI = tuiCopy
				c.Options = optsCopy
			})
		}()

		close(start)
		wg.Wait()

		final := store.Config()
		require.True(t, final.Options.TUI.CompactMode,
			"CompactMode was lost — a concurrent writer's clone overwrote it")
		require.Equal(t, "dark", final.Options.TUI.Theme,
			"Theme was lost — a concurrent writer's clone overwrote it")
	}
}

// TestUpdateConfigVsReload_ReloadDiskStateNotLostByStaleClone is the core
// regression test for the lost-update race between a copy-on-write writer
// (updateConfig) and a concurrent ReloadFromDisk.
//
// Before publishMu unified writers and reloaders under one lock, the
// following interleaving was possible:
//  1. Writer reads snapshot gen-0 (model "model-gen-0"), starts cloning.
//  2. Reload reads snapshot gen-0, rebuilds entirely from disk (which now
//     carries gen-1), publishes gen-1.
//  3. Writer finishes cloning gen-0 + its own mutation, publishes — this
//     Store() is made OVER gen-1 from a gen-0 clone, silently discarding
//     everything the reload brought in from disk.
//
// After the fix, writer and reload are serialised under publishMu: one
// fully completes before the other starts, so the final state always
// reflects the reload's disk state (model "model-gen-1"). The stale-clone
// overwrite (which would leave model "model-gen-0" despite reload having
// physically completed later) can no longer happen.
//
// This test deliberately does NOT rely on a fixed sleep duration to widen
// the race window. An earlier sleep-based version of this test (5-150ms)
// was verified — by hand, against a worktree checked out at the
// pre-publishMu commit — to PASS even on the buggy code: under `go test
// -race`, ReloadFromDisk's own work (config parsing, provider setup) is
// slowed by race instrumentation enough that it reliably takes longer than
// any writer sleep short enough to keep the test fast, so reload always
// happened to publish last regardless of whether the two mutexes were
// unified. That made the sleep-based version pass unconditionally and
// catch nothing. Instead, this version uses channels to force the exact
// interleaving deterministically: the writer blocks INSIDE its mutate
// callback (having already captured its — possibly stale — starting
// snapshot) until explicitly released, so whether ReloadFromDisk can run
// to completion while the writer is still holding the lock is observed
// directly rather than guessed at via timing.
func TestUpdateConfigVsReload_ReloadDiskStateNotLostByStaleClone(t *testing.T) {
	dir := t.TempDir()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", dir)
	t.Setenv("CRUSH_GLOBAL_DATA", dir)
	// Avoid a real network provider-catalog fetch inside ReloadFromDisk:
	// without this, reload's duration is dominated by network I/O (measured
	// up to ~1-2s under -race) instead of the fast, disk-only reload a real
	// deployment sees most of the time — and a slow-for-unrelated-reasons
	// reload would mask this test's race window just as badly as the old
	// sleep-based version did.
	t.Setenv("CRUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	resetProviderState()
	t.Cleanup(resetProviderState)

	configPath := filepath.Join(dir, "crush.json")
	writeGenerationConfig(t, configPath, 0)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// External modification: write gen-1 to disk so the reload will pick up
	// a DIFFERENT model than what's currently in memory.
	writeGenerationConfig(t, configPath, 1)

	writerEnteredMutate := make(chan struct{})
	writerCanFinish := make(chan struct{})
	writerDone := make(chan struct{})

	// Writer: applies an in-memory mutation (no disk write). It captures its
	// starting snapshot (possibly stale gen-0) immediately on entering
	// mutate, then blocks until the test releases it — holding whatever lock
	// updateConfig takes for the ENTIRE blocked duration. If that lock is
	// NOT shared with ReloadFromDisk (the pre-fix bug), the reload below can
	// run to completion while the writer is still parked here; when the
	// writer is finally released, its stale clone overwrites the reload's
	// already-published gen-1.
	go func() {
		store.updateConfig(func(c *Config) {
			close(writerEnteredMutate)
			<-writerCanFinish
			optsCopy := cloneOptions(c.Options)
			optsCopy.Debug = true
			c.Options = optsCopy
		})
		close(writerDone)
	}()

	<-writerEnteredMutate

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- store.ReloadFromDisk(context.Background())
	}()

	// Give the reload a window to complete WHILE the writer is still parked
	// mid-mutate. Under the fix, ReloadFromDisk blocks acquiring the same
	// lock the writer holds, so it cannot complete here — the select falls
	// through to the timeout every time. Under the pre-fix, two-mutex
	// scheme, reload is uncontended by the writer's lock and finishes on its
	// own regardless of the writer, which is exactly the interleaving that
	// causes the bug once the writer is released below.
	reloadFinishedFirst := false
	select {
	case err := <-reloadDone:
		require.NoError(t, err)
		reloadFinishedFirst = true
	case <-time.After(300 * time.Millisecond):
	}

	close(writerCanFinish)
	<-writerDone

	if !reloadFinishedFirst {
		require.NoError(t, <-reloadDone)
	}

	// The reload's disk state MUST be visible: model must be "model-gen-1",
	// not "model-gen-0" (which would mean the writer's stale clone
	// overwrote the reload's fresh-from-disk generation).
	final := store.Config()
	large, ok := final.Models[SelectedModelTypeLarge]
	require.True(t, ok, "large model missing from final config")
	require.Equal(t, "model-gen-1", large.Model,
		"reload's disk state was lost — a writer published a stale clone over the reload's fresh generation (reload finished before writer released=%v)",
		reloadFinishedFirst)
}

// TestConcurrentSetConfigFields_DifferentKeys_BothPersistedOnDisk fires
// two concurrent SetConfigField calls writing DIFFERENT keys to the SAME
// on-disk file. Without diskWriteMu serialising the read-modify-write
// cycle (os.ReadFile → sjson.Set → atomicWriteFile), each goroutine reads
// the pre-write file, applies only its own key, and the second
// atomicWriteFile clobbers the first — a classic lost-update on the file
// itself, independent of the in-memory snapshot race.
//
// With diskWriteMu, the second write blocks until the first completes, so
// it reads the file that already contains the first key and adds its own.
// Both keys end up on disk.
func TestConcurrentSetConfigFields_DifferentKeys_BothPersistedOnDisk(t *testing.T) {
	t.Parallel()

	const iterations = 20
	for range iterations {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "crush.json")
		require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

		cfg := &Config{}
		cfg.setDefaults(dir, "")
		store := newTestConfigStore(testStoreOpts{
			config:         cfg,
			globalDataPath: configPath,
		})

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, store.SetConfigField(ScopeGlobal, "alpha", 1))
		}()

		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, store.SetConfigField(ScopeGlobal, "beta", 2))
		}()

		close(start)
		wg.Wait()

		// Read the file FRESH from disk — NOT from the in-memory snapshot —
		// to verify both writes actually persisted to the file.
		raw, err := os.ReadFile(configPath)
		require.NoError(t, err)

		var onDisk map[string]any
		require.NoError(t, json.Unmarshal(raw, &onDisk))

		_, hasAlpha := onDisk["alpha"]
		require.True(t, hasAlpha, "key alpha was lost — concurrent disk write clobbered it")
		_, hasBeta := onDisk["beta"]
		require.True(t, hasBeta, "key beta was lost — concurrent disk write clobbered it")
	}
}

// TestAutoReload_TryLockSkipsReentry_NoDeadlock verifies that autoReload,
// when called from a context that already holds publishMu (e.g. Load or
// reloadFromDiskLocked → configureProviders → RemoveConfigField), uses
// TryLock and returns immediately instead of blocking forever.
//
// Before the publishMu unification, reloadMu served this role: Load held
// reloadMu, and autoReload's TryLock on reloadMu failed, skipping the
// re-entrant reload. After unification, publishMu serves the same role.
// This test explicitly verifies the guard still works: a regression
// (e.g. switching TryLock to Lock) would deadlock the entire process on
// startup.
func TestAutoReload_TryLockSkipsReentry_NoDeadlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	store := newTestConfigStore(testStoreOpts{config: cfg})
	store.workingDir = dir // Enable the autoReload path past the early return.

	// Hold publishMu, simulating the state during Load or
	// reloadFromDiskLocked when configureProviders calls
	// RemoveConfigField → autoReload.
	store.publishMu.Lock()
	defer store.publishMu.Unlock()

	// autoReload must return nil immediately — TryLock(publishMu) fails
	// because we already hold it. If it used Lock instead of TryLock,
	// this call would block forever.
	done := make(chan error, 1)
	go func() {
		done <- store.autoReload(context.Background())
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "autoReload should return nil when TryLock fails")
	case <-time.After(5 * time.Second):
		t.Fatal("autoReload deadlocked — TryLock was not used or publishMu is not the guard")
	}
}
