package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

type mockCatwalkClient struct {
	providers []catwalk.Provider
	err       error
	callCount int
}

func (m *mockCatwalkClient) GetProviders(ctx context.Context, etag string) ([]catwalk.Provider, error) {
	m.callCount++
	return m.providers, m.err
}

func TestCatwalkSync_Init(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{}
	path := "/tmp/test.json"

	syncer.Init(client, path, true)

	require.True(t, syncer.init.Load())
	require.Equal(t, client, syncer.client)
	require.Equal(t, path, syncer.cache.path)
	require.True(t, syncer.autoupdate)
}

func TestCatwalkSync_GetPanicIfNotInit(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	require.Panics(t, func() {
		_, _ = syncer.Get(t.Context())
	})
}

func TestCatwalkSync_GetWithAutoUpdateDisabled(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		providers: []catwalk.Provider{{Name: "should-not-be-used"}},
	}
	path := t.TempDir() + "/providers.json"

	syncer.Init(client, path, false)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, providers)
	require.Equal(t, 0, client.callCount, "Client should not be called when autoupdate is disabled")

	// Should return embedded providers.
	for _, p := range providers {
		require.NotEqual(t, "should-not-be-used", p.Name)
	}
}

func TestCatwalkSync_GetFreshProviders(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		providers: []catwalk.Provider{
			{Name: "Fresh Provider", ID: "fresh"},
		},
	}
	path := t.TempDir() + "/providers.json"

	syncer.Init(client, path, true)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, "Fresh Provider", providers[0].Name)
	require.Equal(t, 1, client.callCount)

	// Verify cache was written.
	fileInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, fileInfo.IsDir())
}

func TestCatwalkSync_GetNotModifiedUsesCached(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := tmpDir + "/providers.json"

	// Create cache file.
	cachedProviders := []catwalk.Provider{
		{Name: "Cached Provider", ID: "cached"},
	}
	data, err := json.Marshal(cachedProviders)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		err: catwalk.ErrNotModified,
	}

	syncer.Init(client, path, true)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, "Cached Provider", providers[0].Name)
	require.Equal(t, 1, client.callCount)
}

func TestCatwalkSync_GetEmptyResultFallbackToCached(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := tmpDir + "/providers.json"

	// Create cache file.
	cachedProviders := []catwalk.Provider{
		{Name: "Cached Provider", ID: "cached"},
	}
	data, err := json.Marshal(cachedProviders)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		providers: []catwalk.Provider{}, // Empty result.
	}

	syncer.Init(client, path, true)

	providers, err := syncer.Get(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty providers list from catwalk")
	require.Len(t, providers, 1)
	require.Equal(t, "Cached Provider", providers[0].Name)
}

func TestCatwalkSync_GetEmptyCacheDefaultsToEmbedded(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := tmpDir + "/providers.json"

	// Create empty cache file.
	emptyProviders := []catwalk.Provider{}
	data, err := json.Marshal(emptyProviders)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		err: errors.New("network error"),
	}

	syncer.Init(client, path, true)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, providers, "Should fall back to embedded providers")

	// Verify it's embedded providers by checking we have multiple common ones.
	require.Greater(t, len(providers), 5)
}

func TestCatwalkSync_GetClientError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := tmpDir + "/providers.json"

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		err: errors.New("network error"),
	}

	syncer.Init(client, path, true)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err) // Should fall back to embedded.
	require.NotEmpty(t, providers)
}

func TestCatwalkSync_GetCalledMultipleTimesUsesOnce(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		providers: []catwalk.Provider{
			{Name: "Provider", ID: "test"},
		},
	}
	path := t.TempDir() + "/providers.json"

	syncer.Init(client, path, true)

	// Call Get multiple times.
	providers1, err1 := syncer.Get(t.Context())
	require.NoError(t, err1)
	require.Len(t, providers1, 1)

	providers2, err2 := syncer.Get(t.Context())
	require.NoError(t, err2)
	require.Len(t, providers2, 1)

	// Client should only be called once due to sync.Once.
	require.Equal(t, 1, client.callCount)
}

// TestCatwalkSync_GetCacheOnlySkipsNetwork proves that when
// CRUSH_PROVIDER_CACHE_ONLY=1 the syncer serves cached data without
// calling the network client and without rewriting the cache file.
//
// This is the contract `crush models list` (default, no --refresh)
// relies on so a read-only listing has no network/disk side effects.
//
// Not parallel: t.Setenv mutates process-global env, which the testing
// framework forbids inside t.Parallel.
func TestCatwalkSync_GetCacheOnlySkipsNetwork(t *testing.T) {
	// t.Setenv restores the prior value on exit, so this does not leak
	// into other tests in the package.
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")
	// Force TTL=0 in parallel with cache-only to prove cache-only wins
	// over the "always re-fetch" TTL setting.
	t.Setenv("CRUSH_PROVIDER_CACHE_TTL", "0")

	tmpDir := t.TempDir()
	path := tmpDir + "/providers.json"

	// Seed a cache file so we can prove the syncer returns the cached
	// payload verbatim rather than the embedded fallback.
	cachedProviders := []catwalk.Provider{
		{Name: "Cached Provider", ID: "cached"},
	}
	data, err := json.Marshal(cachedProviders)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	infoBefore, err := os.Stat(path)
	require.NoError(t, err)

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		providers: []catwalk.Provider{{Name: "should-not-be-fetched"}},
	}
	syncer.Init(client, path, true)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, "Cached Provider", providers[0].Name, "cache-only must serve cached payload")
	require.Equal(t, 0, client.callCount, "network client must not be called in cache-only mode")

	// Cache file must be untouched (no Store() call).
	infoAfter, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, infoBefore.ModTime(), infoAfter.ModTime(), "cache file must not be rewritten in cache-only mode")
	require.Equal(t, infoBefore.Size(), infoAfter.Size(), "cache file size must not change in cache-only mode")
}

// TestCatwalkSync_GetCacheOnlyFallsBackToEmbedded proves that when
// CRUSH_PROVIDER_CACHE_ONLY=1 and no cache file exists, the syncer
// falls back to the embedded provider list without calling the network
// client or creating a cache file.
//
// Not parallel: t.Setenv mutates process-global env.
func TestCatwalkSync_GetCacheOnlyFallsBackToEmbedded(t *testing.T) {
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")
	t.Setenv("CRUSH_PROVIDER_CACHE_TTL", "0")

	// No cache file at this path.
	path := t.TempDir() + "/providers.json"

	syncer := &catwalkSync{}
	client := &mockCatwalkClient{
		providers: []catwalk.Provider{{Name: "should-not-be-fetched"}},
	}
	syncer.Init(client, path, true)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, providers, "embedded fallback must be returned")
	require.Greater(t, len(providers), 5, "embedded provider list has many entries")
	require.Equal(t, 0, client.callCount, "network client must not be called in cache-only mode")

	// No cache file must have been created.
	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "cache file must not be created in cache-only mode")
}
