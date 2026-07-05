package config

import (
	"context"
	"os"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/require"
)

// zaiKnownProvider returns a Z.AI catwalk provider shaped like the embedded
// default: an OpenAI-compatible provider whose API key template is the
// canonical "$ZAI_API_KEY" env var.
func zaiKnownProvider() catwalk.Provider {
	return catwalk.Provider{
		ID:     catwalk.InferenceProviderZAI,
		Name:   "Z.AI",
		APIKey: "$ZAI_API_KEY",
		Type:   catwalk.TypeOpenAICompat,
		Models: []catwalk.Model{
			{ID: "glm-5", DefaultMaxTokens: 1000},
		},
	}
}

// TestConfigureProviders_ZAIAPIKeyPriority verifies that when both ZAI_API_KEY
// and the ZHIPU_API_KEY fallback are set, the primary ZAI_API_KEY wins. The
// provider is configured and the effective key resolves to the ZAI value.
func TestConfigureProviders_ZAIAPIKeyPriority(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	envMap := env.NewFromMap(map[string]string{
		"ZAI_API_KEY":   "zai-key",
		"ZHIPU_API_KEY": "zhipu-key",
	})
	resolver := NewShellVariableResolver(envMap)

	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, resolver, []catwalk.Provider{zaiKnownProvider()},
	))

	pc, ok := cfg.Providers.Get(string(catwalk.InferenceProviderZAI))
	require.True(t, ok, "zai provider should be configured when ZAI_API_KEY is set")

	// The primary path keeps the unresolved template; resolve it to confirm
	// the effective key is the ZAI value, not the ZHIPU fallback.
	resolved, err := resolver.ResolveValue(pc.APIKey)
	require.NoError(t, err)
	require.Equal(t, "zai-key", resolved)
}

// TestConfigureProviders_ZAIAPIKeyFallback confirms ZHIPU_API_KEY is accepted
// as a fallback when ZAI_API_KEY is not set. The provider is configured with
// the ZHIPU value as a literal key.
func TestConfigureProviders_ZAIAPIKeyFallback(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	envMap := env.NewFromMap(map[string]string{
		"ZHIPU_API_KEY": "zhipu-key",
	})
	resolver := NewShellVariableResolver(envMap)

	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, resolver, []catwalk.Provider{zaiKnownProvider()},
	))

	pc, ok := cfg.Providers.Get(string(catwalk.InferenceProviderZAI))
	require.True(t, ok, "zai provider should be configured via ZHIPU_API_KEY fallback")
	require.Equal(t, "zhipu-key", pc.APIKey)
	require.Equal(t, "zhipu-key", pc.APIKeyTemplate)
}

// TestConfigureProviders_ZAIAPIKeyMissing ensures the Z.AI provider is skipped
// when neither ZAI_API_KEY nor ZHIPU_API_KEY is present.
func TestConfigureProviders_ZAIAPIKeyMissing(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	envMap := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(envMap)

	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, resolver, []catwalk.Provider{zaiKnownProvider()},
	))

	_, ok := cfg.Providers.Get(string(catwalk.InferenceProviderZAI))
	require.False(t, ok, "zai provider should be skipped when no API key is available")
}

// TestConfigureProviders_ZAIConfigAPIKeyWins confirms an explicit
// providers.zai.api_key override in crush.json takes priority over the
// ZHIPU_API_KEY env-var fallback.
func TestConfigureProviders_ZAIConfigAPIKeyWins(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set(string(catwalk.InferenceProviderZAI), ProviderConfig{
		ID:     string(catwalk.InferenceProviderZAI),
		APIKey: "explicit-config-key",
	})
	envMap := env.NewFromMap(map[string]string{
		"ZHIPU_API_KEY": "zhipu-key",
	})
	resolver := NewShellVariableResolver(envMap)

	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, resolver, []catwalk.Provider{zaiKnownProvider()},
	))

	pc, ok := cfg.Providers.Get(string(catwalk.InferenceProviderZAI))
	require.True(t, ok, "zai provider should be configured with an explicit api_key override")

	resolved, err := resolver.ResolveValue(pc.APIKey)
	require.NoError(t, err)
	require.Equal(t, "explicit-config-key", resolved)
}

// TestConfigureProviders_ZAIResolveErrorSkipsNoFallback verifies that when the
// configured Z.AI api_key FAILS to resolve (e.g. a "$(...)" command that
// errors), the provider is skipped rather than silently falling back to
// ZHIPU_API_KEY and masking the misconfiguration. A resolution error is a
// different signal from a cleanly-empty primary: only the latter earns the
// ZHIPU fallback.
func TestConfigureProviders_ZAIResolveErrorSkipsNoFallback(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	// An explicit api_key whose $(...) command fails to resolve (false exits
	// non-zero → command-substitution error in the embedded shell).
	prov := zaiKnownProvider()
	prov.APIKey = "$(false)"
	envMap := env.NewFromMap(map[string]string{
		"PATH":          os.Getenv("PATH"),
		"ZHIPU_API_KEY": "zhipu-key",
	})
	resolver := NewShellVariableResolver(envMap)

	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, resolver, []catwalk.Provider{prov},
	))

	_, ok := cfg.Providers.Get(string(catwalk.InferenceProviderZAI))
	require.False(t, ok, "zai provider must be skipped when its api_key errors, not silently fall back to ZHIPU_API_KEY")
}
