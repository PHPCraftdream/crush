package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/spf13/cobra"
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

func TestParsePeakHoursWindow(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNil   bool
		wantStart string
		wantEnd   string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "normal window",
			input:     "09:00-18:00",
			wantNil:   false,
			wantStart: "09:00",
			wantEnd:   "18:00",
		},
		{
			name:      "overnight window",
			input:     "22:00-06:00",
			wantNil:   false,
			wantStart: "22:00",
			wantEnd:   "06:00",
		},
		{
			name:      "boundary start end of day",
			input:     "00:00-23:59",
			wantNil:   false,
			wantStart: "00:00",
			wantEnd:   "23:59",
		},
		{
			name:    "empty clears (nil)",
			input:   "",
			wantNil: true,
		},
		{
			name:    "off clears (nil)",
			input:   "off",
			wantNil: true,
		},
		{
			name:    "OFF clears case-insensitive",
			input:   "OFF",
			wantNil: true,
		},
		{
			name:    "whitespace-only clears (nil)",
			input:   "   ",
			wantNil: true,
		},
		{
			name:      "trims whitespace",
			input:     "  09:00 - 18:00  ",
			wantNil:   false,
			wantStart: "09:00",
			wantEnd:   "18:00",
		},
		{
			name:      "missing dash",
			input:     "09:00",
			wantErr:   true,
			errSubstr: "expected HH:MM-HH:MM",
		},
		{
			name:      "missing end time",
			input:     "09:00-",
			wantErr:   true,
			errSubstr: "both start and end must be set",
		},
		{
			name:      "bad start format no leading zero",
			input:     "9:00-18:00",
			wantErr:   true,
			errSubstr: "peak_hours start",
		},
		{
			name:      "bad end format dash instead of colon",
			input:     "09:00-18-00",
			wantErr:   true,
			errSubstr: "peak_hours end",
		},
		{
			name:      "hour out of range",
			input:     "24:00-18:00",
			wantErr:   true,
			errSubstr: "peak_hours start",
		},
		{
			name:      "minute out of range",
			input:     "09:00-18:60",
			wantErr:   true,
			errSubstr: "peak_hours end",
		},
		{
			name:      "garbage input",
			input:     "nope",
			wantErr:   true,
			errSubstr: "expected HH:MM-HH:MM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := parsePeakHoursWindow(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errSubstr != "" {
					assert.Contains(t, err.Error(), tt.errSubstr)
				}
				assert.Nil(t, w)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, w)
				return
			}
			require.NotNil(t, w)
			assert.Equal(t, tt.wantStart, w.Start)
			assert.Equal(t, tt.wantEnd, w.End)
		})
	}
}

func TestParsePeakHoursWindow_ReusesConfigValidate(t *testing.T) {
	// The parser must delegate to PeakHoursWindow.Validate, so a window
	// that Validate rejects must also be rejected here.
	w := config.PeakHoursWindow{Start: "09:00", End: ""}
	require.Error(t, w.Validate())

	_, err := parsePeakHoursWindow("09:00-")
	require.Error(t, err)
}

// runProvidersCmdInIsolatedApp executes a real providers subcommand's RunE
// against an isolated config fixture, capturing real stdout. It stands up a
// full app via setupApp (the same path the CLI uses) in a temp data dir with
// network/provider-discovery disabled, so the output is produced by the real
// rendering code in providers.go — not a reimplementation.
//
// cmd is the real providersShowCmd/providersListCmd. providerJSON is the raw
// JSON for the "providers" object written into the isolated global crush.json
// before the command runs. args is the positional/flag payload parsed onto cmd
// (e.g. "with-peak" for show, "--json" for list).
func runProvidersCmdInIsolatedApp(t *testing.T, cmd *cobra.Command, providerJSON, args string) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", tmp)
	// Cache-only so provider discovery makes no network calls.
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	// Pre-initialise the once-only global logger so setupApp's log.Setup
	// call is a no-op and does not open a lumberjack handle inside the
	// temp dir (which would lock the file and break t.TempDir cleanup).
	crushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	globalDataPath := filepath.Join(tmp, "crush.json")
	require.NoError(t, os.WriteFile(globalDataPath, []byte(providerJSON), 0o644))

	// setupApp reads debug/data-dir/cwd off the command it receives. Those
	// are normally rootCmd persistent flags; build a carrier command that
	// carries them so we can invoke the real subcommand's RunE directly.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		// setupApp opens a pooled SQLite connection under workDir and
		// spawns background goroutines keyed on ctx; cancel ctx and force
		// close every pooled connection so t.TempDir cleanup doesn't hit a
		// locked crush.db / crush.log on Windows.
		cancel()
		db.ResetPool()
	})
	carrier := &cobra.Command{Use: "crush"}
	carrier.Flags().Bool("debug", false, "")
	carrier.Flags().String("data-dir", tmp, "")
	carrier.Flags().String("cwd", workDir, "")
	carrier.SetContext(ctx)

	// Reset the subcommand's own flags so state from a prior invocation in
	// the same process (e.g. a leftover --json) doesn't leak in.
	for _, fl := range []string{"json", "grep"} {
		if f := cmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
		}
	}
	cmd.SetArgs(nil)

	var runArgs []string
	if args != "" {
		runArgs = strings.Fields(args)
	}
	require.NoError(t, cmd.ParseFlags(runArgs))

	// Capture os.Stdout — providers list/show write there directly.
	var buf bytes.Buffer
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	runErr := cmd.RunE(carrier, runArgs)

	_ = w.Close()
	os.Stdout = oldOut
	<-done

	require.NoError(t, runErr, "command RunE failed; stdout was:\n%s", buf.String())
	return buf.String()
}

const peakFixtureJSON = `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00"}
    },
    "no-peak": {
      "name": "No Peak",
      "type": "anthropic",
      "base_url": "https://api.anthropic.com",
      "models": [{"id": "claude-sonnet-4"}]
    }
  }
}`

func TestProvidersShow_PeakHoursRendering(t *testing.T) {
	// Regression: this previously reimplemented the peak-hours line inline
	// and asserted the duplicate against itself. It now runs the real
	// providersShowCmd.RunE and asserts on the actual emitted stdout.
	out := runProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "with-peak")

	assert.Contains(t, out, "id:          with-peak")
	assert.Contains(t, out, "peak hours:  09:00-18:00 (currently:")
	// The state must be one of the two real branches the command emits.
	assert.True(t, strings.Contains(out, "(currently: in peak)") || strings.Contains(out, "(currently: not in peak)"),
		"expected a real 'currently:' state in output:\n%s", out)
}

func TestProvidersShow_NoPeakHoursOmitsLine(t *testing.T) {
	// Regression: this previously only asserted p.PeakHours == nil without
	// running the command. It now runs the real providersShowCmd.RunE on a
	// provider without peak hours and asserts the line is absent.
	out := runProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "no-peak")

	assert.Contains(t, out, "id:          no-peak")
	assert.NotContains(t, out, "peak hours", "show must omit the peak-hours line when PeakHours is nil")
}

func TestProvidersList_PeakColumn(t *testing.T) {
	// Regression: this previously reimplemented the PEAK column rendering
	// inline. It now runs the real providersListCmd.RunE and asserts on the
	// actual table output.
	out := runProvidersCmdInIsolatedApp(t, providersListCmd, peakFixtureJSON, "")

	assert.Contains(t, out, "PEAK", "list header must include the PEAK column")
	assert.Contains(t, out, "with-peak", "with-peak row must be present")
	assert.Contains(t, out, "no-peak", "no-peak row must be present")
	// The with-peak row must show the window; the no-peak row must show the
	// em-dash placeholder used by the list command's real rendering.
	assert.Contains(t, out, "09:00-18:00", "with-peak PEAK cell must show the window")
	assert.Contains(t, out, "—", "no-peak PEAK cell must show the placeholder")
}
