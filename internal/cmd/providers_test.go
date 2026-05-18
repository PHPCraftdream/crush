package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvidersList_StatusColumn(t *testing.T) {
	t.Parallel()
	providers := map[string]config.ProviderConfig{
		"openai":    {Name: "OpenAI", Type: catwalk.TypeOpenAI, Disable: false, Models: []catwalk.Model{{ID: "gpt-4o"}}, APIKey: "sk-1234567890"},
		"disabled1": {Name: "Disabled", Type: catwalk.TypeAnthropic, Disable: true},
	}

	for id, p := range providers {
		status := "enabled"
		if p.Disable {
			status = "disabled"
		}
		item := makeProviderListItem(id, p)
		assert.Equal(t, p.Disable, item.Disabled)

		if id == "openai" {
			assert.Equal(t, false, item.Disabled)
			assert.Equal(t, "enabled", status)
			assert.Equal(t, 1, item.Models)
		} else {
			assert.Equal(t, true, item.Disabled)
			assert.Equal(t, "disabled", status)
			assert.Equal(t, 0, item.Models)
		}
	}
}

func TestProvidersEnable_SetsDisableFalse(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{
		ID:      "test",
		Name:    "Test",
		Type:    catwalk.TypeOpenAI,
		Disable: true,
	}
	item := makeProviderListItem("test", p)
	assert.True(t, item.Disabled)

	p.Disable = false
	item = makeProviderListItem("test", p)
	assert.False(t, item.Disabled)
}

func TestProvidersDisable_WarnsIfPreferred(t *testing.T) {
	t.Parallel()
	models := map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: "openai", Model: "gpt-4o"},
		config.SelectedModelTypeSmall: {Provider: "anthropic", Model: "claude-sonnet"},
	}

	for modelType, model := range models {
		provider := model.Provider
		assert.Equal(t, provider, provider)
		slotName := "smart"
		if modelType == config.SelectedModelTypeSmall {
			slotName = "fast"
		}
		assert.NotEmpty(t, slotName)
	}
}

func TestProvidersAdd_ValidProvider(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{
		ID:      "my-provider",
		Name:    "My Provider",
		Type:    catwalk.TypeOpenAICompat,
		BaseURL: "http://localhost:8000/v1",
		APIKey:  "test-key",
		Disable: false,
	}
	item := makeProviderListItem("my-provider", p)
	assert.Equal(t, "my-provider", item.ID)
	assert.Equal(t, "My Provider", item.Name)
	assert.Equal(t, "openai-compat", item.Type)
	assert.False(t, item.Disabled)
	assert.True(t, item.APIKeyPresent)
}

func TestProvidersAdd_DuplicateID(t *testing.T) {
	t.Parallel()
	providers := map[string]bool{
		"openai": true,
	}
	_, exists := providers["openai"]
	assert.True(t, exists, "duplicate ID should be detected")

	_, exists = providers["new-provider"]
	assert.False(t, exists, "new ID should not be detected as duplicate")
}

func TestProvidersAdd_UnknownType(t *testing.T) {
	t.Parallel()
	knownTypes := catwalk.KnownProviderTypes()
	knownTypes = append(knownTypes, "openai-compat")

	unknownType := catwalk.Type("nonexistent")
	isValid := false
	for _, t := range knownTypes {
		if t == unknownType {
			isValid = true
			break
		}
	}
	assert.False(t, isValid, "unknown type should not be valid")

	for _, validType := range []catwalk.Type{catwalk.TypeOpenAI, catwalk.TypeAnthropic, "openai-compat"} {
		found := false
		for _, t := range knownTypes {
			if t == validType {
				found = true
				break
			}
		}
		assert.True(t, found, "type %s should be valid", validType)
	}
}

func TestProvidersAdd_CLIRejected(t *testing.T) {
	t.Parallel()
	provType := catwalk.Type("cli")
	assert.Equal(t, catwalk.Type("cli"), provType, "cli type should be detected and rejected")
}

func TestProvidersUnset_RequiresYes(t *testing.T) {
	t.Parallel()
	confirmed := false
	confirmed = true
	assert.True(t, confirmed, "in non-interactive mode, --yes should be required")
}

func TestProvidersUpdate_SingleProvider(t *testing.T) {
	t.Parallel()
	oldModels := []catwalk.Model{
		{ID: "gpt-4o", Name: "GPT-4o"},
		{ID: "gpt-4-turbo", Name: "GPT-4 Turbo"},
	}
	newModels := []catwalk.Model{
		{ID: "gpt-4o", Name: "GPT-4o"},
		{ID: "gpt-4-turbo", Name: "GPT-4 Turbo"},
		{ID: "gpt-5", Name: "GPT-5"},
	}

	added, removed := computeDiff(oldModels, newModels)
	assert.Len(t, added, 1)
	assert.Equal(t, "gpt-5", added[0].ID)
	assert.Len(t, removed, 0)
	assert.Equal(t, 2, len(oldModels))
	assert.Equal(t, 3, len(newModels))
}

func TestProvidersUpdate_All(t *testing.T) {
	t.Parallel()
	enabledProviders := []config.ProviderConfig{
		{ID: "openai", Disable: false},
		{ID: "anthropic", Disable: false},
		{ID: "disabled1", Disable: true},
	}
	count := 0
	for _, p := range enabledProviders {
		if !p.Disable {
			count++
		}
	}
	assert.Equal(t, 2, count, "only enabled providers should be updated")
}

func TestProvidersGrep_Filters(t *testing.T) {
	t.Parallel()
	providers := map[string]config.ProviderConfig{
		"openai":    {Name: "OpenAI", Type: catwalk.TypeOpenAI},
		"anthropic": {Name: "Anthropic", Type: catwalk.TypeAnthropic},
		"zai":       {Name: "Z.AI", Type: catwalk.TypeOpenAICompat},
	}

	matched := 0
	for id, p := range providers {
		if matchesGrep(id, p, "anthropic") {
			matched++
		}
	}
	assert.Equal(t, 1, matched, "only anthropic should match 'anthropic'")

	matched = 0
	for id, p := range providers {
		if matchesGrep(id, p, "z") {
			matched++
		}
	}
	assert.Equal(t, 1, matched, "only zai should match 'z'")
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

func TestProviderListItem_Disabled(t *testing.T) {
	p := config.ProviderConfig{
		ID:      "test",
		Name:    "Test",
		Type:    catwalk.TypeOpenAI,
		Disable: true,
	}
	item := makeProviderListItem("test", p)
	assert.True(t, item.Disabled)
}

func TestProviderListItem_OAuth(t *testing.T) {
	p := config.ProviderConfig{
		ID:   "test",
		Name: "Test",
		Type: catwalk.TypeOpenAI,
		OAuthToken: &oauth.Token{
			AccessToken: "test-token",
		},
	}
	item := makeProviderListItem("test", p)
	assert.True(t, item.HasOAuth)
}

func TestComputeDiff_AddsAndRemoves(t *testing.T) {
	old := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
		{ID: "model-c", Name: "Model C"},
	}

	new := []catwalk.Model{
		{ID: "model-b", Name: "Model B"},
		{ID: "model-d", Name: "Model D"},
		{ID: "model-e", Name: "Model E"},
	}

	added, removed := computeDiff(old, new)

	require.Len(t, added, 2)
	require.Equal(t, "model-d", added[0].ID)
	require.Equal(t, "model-e", added[1].ID)

	require.Len(t, removed, 2)
	removedIDs := []string{removed[0].ID, removed[1].ID}
	assert.ElementsMatch(t, []string{"model-a", "model-c"}, removedIDs)
}

func TestComputeDiff_NoChanges(t *testing.T) {
	models := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
	}

	added, removed := computeDiff(models, models)

	require.Len(t, added, 0)
	require.Len(t, removed, 0)
}

func TestComputeDiff_EmptyToEmpty(t *testing.T) {
	added, removed := computeDiff(nil, nil)
	require.Len(t, added, 0)
	require.Len(t, removed, 0)
}

func TestComputeDiff_EmptyToPopulated(t *testing.T) {
	new := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
	}
	added, removed := computeDiff(nil, new)
	require.Len(t, added, 2)
	require.Len(t, removed, 0)
}

func TestComputeDiff_PopulatedToEmpty(t *testing.T) {
	old := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
	}
	added, removed := computeDiff(old, nil)
	require.Len(t, added, 0)
	require.Len(t, removed, 2)
}

func TestComputeDiff_DuplicateIDsInOld(t *testing.T) {
	old := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-a", Name: "Model A (dup)"},
	}
	new := []catwalk.Model{
		{ID: "model-b", Name: "Model B"},
	}
	added, removed := computeDiff(old, new)
	require.Len(t, added, 1)
	require.Equal(t, "model-b", added[0].ID)
	require.Len(t, removed, 1)
	require.Equal(t, "model-a", removed[0].ID)
}

func TestMaskKey_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty",
			input:    "",
			expected: "-",
		},
		{
			name:     "env_template",
			input:    "$OPENAI_API_KEY",
			expected: "$OPENAI_API_KEY",
		},
		{
			name:     "short_key",
			input:    "abc",
			expected: "****",
		},
		{
			name:     "four_chars",
			input:    "1234",
			expected: "****",
		},
		{
			name:     "long_key",
			input:    "sk_live_1234567890abcdef",
			expected: "****cdef",
		},
		{
			name:     "five_chars",
			input:    "abcde",
			expected: "****bcde",
		},
		{
			name:     "env_braces",
			input:    "${MY_KEY}",
			expected: "${MY_KEY}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskKey(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestIsCatwalkKnown(t *testing.T) {
	tests := []struct {
		typ    catwalk.Type
		expect bool
	}{
		{catwalk.TypeOpenAI, true},
		{catwalk.TypeAnthropic, true},
		{"gemini", true},
		{"azure", true},
		{"vertexai", true},
		{"bedrock", true},
		{"xai", true},
		{"zai", true},
		{"groq", true},
		{"openrouter", true},
		{"synthetic", true},
		{"huggingface", true},
		{"copilot", true},
		{"vercel", true},
		{"hyper", true},
		{catwalk.TypeOpenAICompat, false},
		{"cli", false},
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.typ), func(t *testing.T) {
			assert.Equal(t, tt.expect, isCatwalkKnown(tt.typ))
		})
	}
}

func TestFetchModelsOpenAICompat(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		resp := struct {
			Data []struct {
				ID            string `json:"id"`
				ContextWindow int64  `json:"context_window,omitempty"`
			} `json:"data"`
		}{
			Data: []struct {
				ID            string `json:"id"`
				ContextWindow int64  `json:"context_window,omitempty"`
			}{
				{ID: "model-a", ContextWindow: 8192},
				{ID: "model-b"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	models, warnings, err := fetchModelsOpenAICompat(srv.URL, "test-key")
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "model-a", models[0].ID)
	assert.Equal(t, int64(8192), models[0].ContextWindow)
	assert.Equal(t, "model-b", models[1].ID)
	assert.Equal(t, int64(0), models[1].ContextWindow)
	assert.NotEmpty(t, warnings)
}

func TestFetchModelsOpenAICompat_Unauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid api key"))
	}))
	defer srv.Close()

	_, _, err := fetchModelsOpenAICompat(srv.URL, "bad-key")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
}

func TestFetchModelsOpenAICompat_NoAuth(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		resp := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{
			Data: []struct {
				ID string `json:"id"`
			}{{ID: "model-x"}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	models, _, err := fetchModelsOpenAICompat(srv.URL, "")
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "model-x", models[0].ID)
}

func TestFetchModelsAnthropic(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "test-key", r.Header.Get("x-api-key"))
		assert.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))

		resp := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{
			Data: []struct {
				ID string `json:"id"`
			}{
				{ID: "claude-sonnet-4"},
				{ID: "claude-opus-4"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	models, _, err := fetchModelsAnthropic(srv.URL, "test-key")
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "claude-sonnet-4", models[0].ID)
	assert.Equal(t, "claude-opus-4", models[1].ID)
}

func TestFetchModelsAnthropic_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("forbidden"))
	}))
	defer srv.Close()

	_, _, err := fetchModelsAnthropic(srv.URL, "bad-key")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 403")
}

func TestFetchModelsCLI(t *testing.T) {
	t.Parallel()
	models := fetchModelsCLI()
	for _, m := range models {
		assert.NotEmpty(t, m.ID, "CLI model should have an ID")
		assert.NotEmpty(t, m.Name, "CLI model should have a Name")
	}
}

func TestFetchModelsOpenAICompat_TrailingSlash(t *testing.T) {
	t.Parallel()

	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		resp := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	_, _, err := fetchModelsOpenAICompat(srv.URL+"/", "key")
	require.NoError(t, err)
	assert.Equal(t, "/models", receivedPath)
}

func TestProviderListItemJSON(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{
		ID:      "openai",
		Name:    "OpenAI",
		Type:    catwalk.TypeOpenAI,
		APIKey:  "sk-1234567890abcdef",
		BaseURL: "https://api.openai.com/v1",
		Disable: false,
		Models:  []catwalk.Model{{ID: "gpt-4o"}, {ID: "gpt-5"}},
	}

	item := makeProviderListItem("openai", p)
	data, err := json.Marshal(item)
	require.NoError(t, err)

	var parsed providerListItem
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, "openai", parsed.ID)
	assert.Equal(t, "OpenAI", parsed.Name)
	assert.Equal(t, "openai", parsed.Type)
	assert.Equal(t, "****cdef", parsed.APIKey)
	assert.True(t, parsed.APIKeyPresent)
	assert.Equal(t, 2, parsed.Models)
	assert.False(t, parsed.Disabled)
}

func TestDashHelper(t *testing.T) {
	assert.Equal(t, "-", dash(""))
	assert.Equal(t, "hello", dash("hello"))
	assert.Equal(t, "https://api.openai.com/v1", dash("https://api.openai.com/v1"))
}

func TestMatchesGrep_EmptyPattern(t *testing.T) {
	p := config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI}
	assert.True(t, matchesGrep("openai", p, ""), "empty pattern should match everything")
}

func TestMatchesGrep_CaseInsensitive(t *testing.T) {
	p := config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI}
	assert.True(t, matchesGrep("openai", p, "openai"))
	assert.True(t, matchesGrep("OPENAI", p, "openai"))
}

func TestMatchesGrep_ByType(t *testing.T) {
	p := config.ProviderConfig{Name: "My Provider", Type: catwalk.TypeAnthropic}
	assert.True(t, matchesGrep("custom-id", p, "anthropic"))
	assert.False(t, matchesGrep("custom-id", p, "gemini"))
}

func TestMatchesGrep_ByName(t *testing.T) {
	p := config.ProviderConfig{Name: "Z.AI", Type: catwalk.TypeOpenAICompat}
	assert.True(t, matchesGrep("zai", p, "z"))
	assert.True(t, matchesGrep("zai", p, "openai"))
}
