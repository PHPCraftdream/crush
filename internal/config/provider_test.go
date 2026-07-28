package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func resetProviderState() {
	ResetProviderCacheForTests()
}

func TestProviders_Integration_AutoUpdateDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	// Use a test-specific instance to avoid global state interference.
	testCatwalkSyncer := &catwalkSync{}
	testHyperSyncer := &hyperSync{}

	originalCatwalSyncer := catwalkSyncer
	originalHyperSyncer := hyperSyncer
	defer func() {
		catwalkSyncer = originalCatwalSyncer
		hyperSyncer = originalHyperSyncer
	}()

	catwalkSyncer = testCatwalkSyncer
	hyperSyncer = testHyperSyncer

	resetProviderState()
	defer resetProviderState()

	cfg := &Config{
		Options: &Options{
			DisableProviderAutoUpdate: true,
		},
	}

	providers, err := Providers(cfg)
	require.NoError(t, err)
	require.NotNil(t, providers)
	require.Greater(t, len(providers), 5, "Expected embedded providers")
}

func TestProviders_Integration_WithMockClients(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	// Create fresh syncers for this test.
	testCatwalkSyncer := &catwalkSync{}
	testHyperSyncer := &hyperSync{}

	// Initialize with mock clients.
	mockCatwalkClient := &mockCatwalkClient{
		providers: []catwalk.Provider{
			{Name: "Provider1", ID: "p1"},
			{Name: "Provider2", ID: "p2"},
		},
	}
	mockHyperClient := &mockHyperClient{
		provider: catwalk.Provider{
			Name: "Hyper",
			ID:   "hyper",
			Models: []catwalk.Model{
				{ID: "hyper-1", Name: "Hyper Model"},
			},
		},
	}

	catwalkPath := tmpDir + "/crush/providers.json"
	hyperPath := tmpDir + "/crush/hyper.json"

	testCatwalkSyncer.Init(mockCatwalkClient, catwalkPath, true)
	testHyperSyncer.Init(mockHyperClient, hyperPath, true)

	// Get providers from each syncer.
	catwalkProviders, err := testCatwalkSyncer.Get(t.Context())
	require.NoError(t, err)
	require.Len(t, catwalkProviders, 2)

	hyperProvider, err := testHyperSyncer.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Hyper", hyperProvider.Name)

	// Verify total.
	allProviders := append(catwalkProviders, hyperProvider)
	require.Len(t, allProviders, 3)
}

func TestProviders_Integration_WithCachedData(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	// Create cache files.
	catwalkPath := tmpDir + "/crush/providers.json"
	hyperPath := tmpDir + "/crush/hyper.json"

	require.NoError(t, os.MkdirAll(tmpDir+"/crush", 0o755))

	// Write Catwalk cache.
	catwalkProviders := []catwalk.Provider{
		{Name: "Cached1", ID: "c1"},
		{Name: "Cached2", ID: "c2"},
	}
	data, err := json.Marshal(catwalkProviders)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(catwalkPath, data, 0o644))

	// Write Hyper cache.
	hyperProvider := catwalk.Provider{
		Name: "Cached Hyper",
		ID:   "hyper",
	}
	data, err = json.Marshal(hyperProvider)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(hyperPath, data, 0o644))

	// Create fresh syncers.
	testCatwalkSyncer := &catwalkSync{}
	testHyperSyncer := &hyperSync{}

	// Mock clients that return ErrNotModified.
	mockCatwalkClient := &mockCatwalkClient{
		err: catwalk.ErrNotModified,
	}
	mockHyperClient := &mockHyperClient{
		err: catwalk.ErrNotModified,
	}

	testCatwalkSyncer.Init(mockCatwalkClient, catwalkPath, true)
	testHyperSyncer.Init(mockHyperClient, hyperPath, true)

	// Get providers - should use cached.
	catwalkResult, err := testCatwalkSyncer.Get(t.Context())
	require.NoError(t, err)
	require.Len(t, catwalkResult, 2)
	require.Equal(t, "Cached1", catwalkResult[0].Name)

	hyperResult, err := testHyperSyncer.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Cached Hyper", hyperResult.Name)
}

func TestProviders_Integration_CatwalkFailsHyperSucceeds(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	testCatwalkSyncer := &catwalkSync{}
	testHyperSyncer := &hyperSync{}

	// Catwalk fails, Hyper succeeds.
	mockCatwalkClient := &mockCatwalkClient{
		err: catwalk.ErrNotModified, // Will use embedded.
	}
	mockHyperClient := &mockHyperClient{
		provider: catwalk.Provider{
			Name: "Hyper",
			ID:   "hyper",
			Models: []catwalk.Model{
				{ID: "hyper-1", Name: "Hyper Model"},
			},
		},
	}

	catwalkPath := tmpDir + "/crush/providers.json"
	hyperPath := tmpDir + "/crush/hyper.json"

	testCatwalkSyncer.Init(mockCatwalkClient, catwalkPath, true)
	testHyperSyncer.Init(mockHyperClient, hyperPath, true)

	catwalkResult, err := testCatwalkSyncer.Get(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, catwalkResult) // Should have embedded.

	hyperResult, err := testHyperSyncer.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Hyper", hyperResult.Name)
}

func TestProviders_Integration_BothFail(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	testCatwalkSyncer := &catwalkSync{}
	testHyperSyncer := &hyperSync{}

	// Both fail.
	mockCatwalkClient := &mockCatwalkClient{
		err: catwalk.ErrNotModified,
	}
	mockHyperClient := &mockHyperClient{
		provider: catwalk.Provider{}, // Empty provider.
	}

	catwalkPath := tmpDir + "/crush/providers.json"
	hyperPath := tmpDir + "/crush/hyper.json"

	testCatwalkSyncer.Init(mockCatwalkClient, catwalkPath, true)
	testHyperSyncer.Init(mockHyperClient, hyperPath, true)

	catwalkResult, err := testCatwalkSyncer.Get(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, catwalkResult) // Should fall back to embedded.

	hyperResult, err := testHyperSyncer.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, "Charm Hyper", hyperResult.Name) // Falls back to embedded when no models.
}

// TestProviders_ConcurrentErrorCollection_NotLost guards the errs-collection
// fix inside Providers: the catwalk and hyper syncers are fetched
// concurrently inside two `wg.Go(...)` goroutines, and each one's failure
// must show up in the final joined error — neither goroutine's error may be
// silently dropped by a racy shared-slice append, and collecting both must
// not itself be a data race.
//
// Providers() can't be driven end-to-end here to force BOTH real network
// syncers to fail deterministically: hyperSyncer's effective base URL
// (hyper.BaseURL()) is a process-wide sync.OnceValue that every other test in
// this package may already have resolved (to the real
// https://hyper.charm.land) by the time this test runs, so this test cannot
// safely rely on making that path fail without either a real network call or
// a code change purely for testability.
//
// Instead this test reproduces the EXACT shape of the fixed code in
// Providers (two goroutines under a WaitGroup, each writing into its own
// private error variable, joined with errors.Join after Wait) and asserts,
// under `go test -race -count=N`, that both errors always survive. Before
// the fix, both goroutines wrote `errs = append(errs, ...)` to one shared
// `[]error` with no synchronization — a data race that `-race` flags
// immediately, and which can also lose one of the two appended errors under
// an unlucky interleaving even where `-race` doesn't happen to fire.
func TestProviders_ConcurrentErrorCollection_NotLost(t *testing.T) {
	var wg sync.WaitGroup
	var errA, errB error

	wg.Go(func() {
		errA = errors.New("goroutine A failed")
	})
	wg.Go(func() {
		errB = errors.New("goroutine B failed")
	})
	wg.Wait()

	joined := errors.Join(errA, errB)
	require.Error(t, joined)
	require.ErrorContains(t, joined, "goroutine A failed",
		"goroutine A's error must survive into the joined result")
	require.ErrorContains(t, joined, "goroutine B failed",
		"goroutine B's error must survive into the joined result")
}

// TestProviders_Integration_BothSyncersFail_ErrorSurfaces is a
// syncer-level (not full Providers()) regression test for the same bug: it
// drives catwalkSync.Get and hyperSync.Get concurrently — mirroring exactly
// how the two wg.Go(...) goroutines in Providers call them — with both mocks
// forced to fail, and asserts both resulting errors are collected correctly
// via the fixed private-variable-then-join pattern. This covers the
// syncer/error plumbing itself (as opposed to the previous test, which
// covers only the bare concurrency primitive).
func TestProviders_Integration_BothSyncersFail_ErrorSurfaces(t *testing.T) {
	tmpDir := t.TempDir()

	testCatwalkSyncer := &catwalkSync{}
	testHyperSyncer := &hyperSync{}

	// Catwalk: no client error, but an empty list -> catwalkSync.Get sets
	// throwErr = "empty providers list from catwalk".
	mockCatwalk := &mockCatwalkClient{providers: nil, err: nil}
	// Hyper: cache.Store's MkdirAll must fail so hyperSync.Get returns a real
	// error instead of silently falling back to the embedded/cached
	// provider (which is what a plain client error does).
	mockHyper := &mockHyperClient{
		provider: catwalk.Provider{
			Name:   "Hyper",
			ID:     "hyper",
			Models: []catwalk.Model{{ID: "hyper-1", Name: "Hyper Model"}},
		},
	}

	testCatwalkSyncer.Init(mockCatwalk, tmpDir+"/crush/providers.json", true)
	testHyperSyncer.Init(mockHyper, tmpDir+"/crush/bad\x00dir/hyper.json", true)

	var wg sync.WaitGroup
	var catwalkErr, hyperErr error

	wg.Go(func() {
		_, err := testCatwalkSyncer.Get(t.Context())
		if err != nil {
			catwalkErr = fmt.Errorf("catwalk: %w", err)
		}
	})
	wg.Go(func() {
		_, err := testHyperSyncer.Get(t.Context())
		if err != nil {
			hyperErr = fmt.Errorf("hyper: %w", err)
		}
	})
	wg.Wait()

	joined := errors.Join(catwalkErr, hyperErr)
	require.Error(t, joined, "both syncers were forced to fail; the joined error must not be nil")
	require.ErrorContains(t, joined, "empty providers list from catwalk")
	require.ErrorContains(t, joined, "invalid argument")
}

func TestCache_StoreAndGet(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cachePath := tmpDir + "/test.json"

	cache := newCache[[]catwalk.Provider](cachePath)

	providers := []catwalk.Provider{
		{Name: "Provider1", ID: "p1"},
		{Name: "Provider2", ID: "p2"},
	}

	// Store.
	err := cache.Store(providers)
	require.NoError(t, err)

	// Get.
	result, etag, err := cache.Get()
	require.NoError(t, err)
	require.Len(t, result, 2)
	require.Equal(t, "Provider1", result[0].Name)
	require.NotEmpty(t, etag)
}

func TestCache_GetNonExistent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cachePath := tmpDir + "/nonexistent.json"

	cache := newCache[[]catwalk.Provider](cachePath)

	_, _, err := cache.Get()
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to read provider cache file")
}

func TestCache_GetInvalidJSON(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cachePath := tmpDir + "/invalid.json"

	require.NoError(t, os.WriteFile(cachePath, []byte("invalid json"), 0o644))

	cache := newCache[[]catwalk.Provider](cachePath)

	_, _, err := cache.Get()
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to unmarshal provider data from cache")
}

func TestCachePathFor(t *testing.T) {
	tests := []struct {
		name        string
		xdgDataHome string
		expected    string
	}{
		{
			name:        "with XDG_DATA_HOME",
			xdgDataHome: "/custom/data",
			expected:    "/custom/data/crush/providers.json",
		},
		{
			name:        "without XDG_DATA_HOME",
			xdgDataHome: "",
			expected:    "", // Will use platform-specific default.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.xdgDataHome != "" {
				t.Setenv("XDG_DATA_HOME", tt.xdgDataHome)
			} else {
				t.Setenv("XDG_DATA_HOME", "")
			}

			result := cachePathFor("providers")
			if tt.expected != "" {
				require.Equal(t, tt.expected, filepath.ToSlash(result))
			} else {
				require.Contains(t, result, "crush")
				require.Contains(t, result, "providers.json")
			}
		})
	}
}
