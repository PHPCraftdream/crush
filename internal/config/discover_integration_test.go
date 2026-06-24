package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/require"
)

// newModelsServer returns an httptest server that answers the OpenAI-compat
// /v1/models listing with the given model IDs.
func newModelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[`))
		for i, id := range ids {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(`{"id":"` + id + `","object":"model"}`))
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestConfigureProviders_AutoDiscovers verifies that a custom provider with
// no models list and a reachable /v1/models endpoint gets its models
// auto-populated by discovery.
func TestConfigureProviders_AutoDiscovers(t *testing.T) {
	srv := newModelsServer(t, "model-a", "model-b")

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				APIKey:  "test-key",
				BaseURL: srv.URL + "/v1",
				Type:    catwalk.TypeOpenAICompat,
				// No Models → auto-discovery trigger.
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	e := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(e)
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), e, resolver, []catwalk.Provider{}))

	pc, ok := cfg.Providers.Get("custom")
	require.True(t, ok, "provider should survive after discovery")
	ids := make([]string, len(pc.Models))
	for i, m := range pc.Models {
		ids[i] = m.ID
	}
	require.ElementsMatch(t, []string{"model-a", "model-b"}, ids)
}

// TestConfigureProviders_DiscoveryOptOut verifies that discover_models=false
// disables auto-discovery: a provider with no models is removed rather than
// populated from the endpoint.
func TestConfigureProviders_DiscoveryOptOut(t *testing.T) {
	srv := newModelsServer(t, "model-a")

	optOut := false
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				APIKey:             "test-key",
				BaseURL:            srv.URL + "/v1",
				Type:               catwalk.TypeOpenAICompat,
				AutoDiscoverModels: &optOut,
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	e := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(e)
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), e, resolver, []catwalk.Provider{}))

	_, ok := cfg.Providers.Get("custom")
	require.False(t, ok, "opted-out provider with no models should be removed")
}

// TestConfigureProviders_DiscoveryMergesWithExisting verifies that
// discover_models=true with an explicit model list merges discovered models
// in while keeping the user-specified ones.
func TestConfigureProviders_DiscoveryMergesWithExisting(t *testing.T) {
	srv := newModelsServer(t, "discovered-1", "user-model")

	optIn := true
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				APIKey:             "test-key",
				BaseURL:            srv.URL + "/v1",
				Type:               catwalk.TypeOpenAICompat,
				AutoDiscoverModels: &optIn,
				Models:             []catwalk.Model{{ID: "user-model", Name: "User Model"}},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	e := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(e)
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), e, resolver, []catwalk.Provider{}))

	pc, ok := cfg.Providers.Get("custom")
	require.True(t, ok)
	var userModel catwalk.Model
	ids := make([]string, len(pc.Models))
	for i, m := range pc.Models {
		ids[i] = m.ID
		if m.ID == "user-model" {
			userModel = m
		}
	}
	require.ElementsMatch(t, []string{"user-model", "discovered-1"}, ids)
	// User-specified model metadata must win over the discovered bare entry.
	require.Equal(t, "User Model", userModel.Name)
}
