package app

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeAppForResolve builds a bare *App whose Config exposes the given
// providers — enough for ResolveModel to do its lookup without spinning
// up DB / coordinator / agents.
func makeAppForResolve(t *testing.T, providers map[string]config.ProviderConfig) *App {
	t.Helper()
	store, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfg := store.Config()
	cfg.Providers = csync.NewMap[string, config.ProviderConfig]()
	for id, p := range providers {
		cfg.Providers.Set(id, p)
	}
	return &App{config: store}
}

func TestResolveModel_ExactProviderSlashModel(t *testing.T) {
	a := makeAppForResolve(t, map[string]config.ProviderConfig{
		"openai": {ID: "openai", Models: []catwalk.Model{{ID: "gpt-5"}}},
		"alt":    {ID: "alt", Models: []catwalk.Model{{ID: "gpt-5"}}}, // dup name
	})
	p, m, err := a.ResolveModel("openai/gpt-5")
	require.NoError(t, err)
	assert.Equal(t, "openai", p)
	assert.Equal(t, "gpt-5", m)
}

func TestResolveModel_BareName_Unique(t *testing.T) {
	a := makeAppForResolve(t, map[string]config.ProviderConfig{
		"openai": {ID: "openai", Models: []catwalk.Model{{ID: "gpt-5"}, {ID: "gpt-4o-mini"}}},
		"anth":   {ID: "anth", Models: []catwalk.Model{{ID: "claude"}}},
	})
	p, m, err := a.ResolveModel("claude")
	require.NoError(t, err)
	assert.Equal(t, "anth", p)
	assert.Equal(t, "claude", m)
}

func TestResolveModel_BareName_AmbiguityIsError(t *testing.T) {
	a := makeAppForResolve(t, map[string]config.ProviderConfig{
		"openai": {ID: "openai", Models: []catwalk.Model{{ID: "gpt-5"}}},
		"alt":    {ID: "alt", Models: []catwalk.Model{{ID: "gpt-5"}}},
	})
	_, _, err := a.ResolveModel("gpt-5")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "found in multiple providers")
}

func TestResolveModel_NotFound(t *testing.T) {
	a := makeAppForResolve(t, map[string]config.ProviderConfig{
		"openai": {ID: "openai", Models: []catwalk.Model{{ID: "gpt-5"}}},
	})
	_, _, err := a.ResolveModel("nonexistent-model")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolveModel_UnknownProviderPrefix(t *testing.T) {
	a := makeAppForResolve(t, map[string]config.ProviderConfig{
		"openai": {ID: "openai", Models: []catwalk.Model{{ID: "gpt-5"}}},
	})
	_, _, err := a.ResolveModel("ghost/whatever")
	require.Error(t, err)
	// parseModelStr falls through to treating the whole string as a model
	// id when the prefix isn't a known provider, so the message says
	// "not found", not "provider not found". Either is fine — assert on
	// the part that's stable.
	assert.Contains(t, err.Error(), "not found")
}

func TestResolveModel_SkipsDisabledProviders(t *testing.T) {
	a := makeAppForResolve(t, map[string]config.ProviderConfig{
		"on":  {ID: "on", Models: []catwalk.Model{{ID: "shared"}}},
		"off": {ID: "off", Disable: true, Models: []catwalk.Model{{ID: "shared"}}},
	})
	p, m, err := a.ResolveModel("shared")
	require.NoError(t, err)
	assert.Equal(t, "on", p, "disabled providers must not contribute to matches")
	assert.Equal(t, "shared", m)
}
