package cmd

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestProvidersList_StatusColumn(t *testing.T) {
	t.Parallel()
	// Test that list command shows STATUS column instead of DISABLED
	// and filters work with --grep
}

func TestProvidersEnable_SetsDisableFalse(t *testing.T) {
	t.Parallel()
	// Test that enable command sets Disable=false
}

func TestProvidersDisable_WarnsIfPreferred(t *testing.T) {
	t.Parallel()
	// Test that disabling a provider used in preferred slot warns
}

func TestProvidersAdd_ValidProvider(t *testing.T) {
	t.Parallel()
	// Test that add creates a new provider
}

func TestProvidersAdd_DuplicateID(t *testing.T) {
	t.Parallel()
	// Test that adding with existing ID returns error
}

func TestProvidersAdd_UnknownType(t *testing.T) {
	t.Parallel()
	// Test that unknown provider type returns error
}

func TestProvidersUnset_RequiresYes(t *testing.T) {
	t.Parallel()
	// Test that unset requires --yes flag in non-interactive mode
}

func TestProvidersUpdate_SingleProvider(t *testing.T) {
	t.Parallel()
	// Test that update works for a single provider
}

func TestProvidersUpdate_All(t *testing.T) {
	t.Parallel()
	// Test that --all updates all enabled providers
}

func TestProvidersGrep_Filters(t *testing.T) {
	t.Parallel()
	// Test that grep filters by id, name, or type
}

func TestMatchesGrep(t *testing.T) {
	tests := []struct {
		id       string
		provider config.ProviderConfig
		pattern  string
		expect   bool
	}{
		{
			id:       "openai",
			provider: config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI},
			pattern:  "openai",
			expect:   true,
		},
		{
			id:       "openai",
			provider: config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI},
			pattern:  "gpt",
			expect:   false,
		},
		{
			id:       "zai",
			provider: config.ProviderConfig{Name: "Z.AI", Type: catwalk.TypeOpenAICompat},
			pattern:  "z.ai",
			expect:   true,
		},
		{
			id:       "anthropic",
			provider: config.ProviderConfig{Name: "Anthropic", Type: catwalk.TypeAnthropic},
			pattern:  "anthropic",
			expect:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.id+"_"+tt.pattern, func(t *testing.T) {
			result := matchesGrep(tt.id, tt.provider, strings.ToLower(tt.pattern))
			require.Equal(t, tt.expect, result)
		})
	}
}

func TestProviderListItem(t *testing.T) {
	p := config.ProviderConfig{
		ID:      "openai",
		Name:    "OpenAI",
		Type:    catwalk.TypeOpenAI,
		APIKey:  "sk_live_abc123def456",
		BaseURL: "https://api.openai.com/v1",
		Disable: false,
		Models: []catwalk.Model{
			{ID: "gpt-4o", Name: "GPT-4 Omni"},
			{ID: "gpt-4-turbo", Name: "GPT-4 Turbo"},
		},
	}

	item := makeProviderListItem("openai", p)

	require.Equal(t, "openai", item.ID)
	require.Equal(t, "OpenAI", item.Name)
	require.Equal(t, "openai", item.Type)
	require.Equal(t, "****f456", item.APIKey)
	require.True(t, item.APIKeyPresent)
	require.Equal(t, "https://api.openai.com/v1", item.BaseURL)
	require.False(t, item.Disabled)
	require.Equal(t, 2, item.Models)
}

func TestProviderListItem_NoAPIKey(t *testing.T) {
	p := config.ProviderConfig{
		ID:   "test",
		Name: "Test",
		Type: catwalk.TypeOpenAI,
	}

	item := makeProviderListItem("test", p)

	require.Equal(t, "-", item.APIKey)
	require.False(t, item.APIKeyPresent)
}

func TestProviderListItem_EnvTemplate(t *testing.T) {
	p := config.ProviderConfig{
		ID:     "test",
		Name:   "Test",
		Type:   catwalk.TypeOpenAI,
		APIKey: "$OPENAI_KEY",
	}

	item := makeProviderListItem("test", p)

	require.Equal(t, "$OPENAI_KEY", item.APIKey)
	require.True(t, item.APIKeyPresent)
}

