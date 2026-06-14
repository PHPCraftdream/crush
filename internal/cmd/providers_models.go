// Fork patch: batch 12 — model-fetch helpers for `crush providers update`.
// Lives next to providers.go (its single caller is updateSingleProvider in
// providers.go). Restored after a stray manual delete — see commit log.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
)

// fetchModels fetches the model list for a provider based on its type.
// Returns the updated model list and any warning messages.
func fetchModels(a *app.App, p config.ProviderConfig) ([]catwalk.Model, []string, error) {
	var warnings []string

	// Resolve env-template API keys (e.g. "$ZAI_API_KEY") before HTTP calls —
	// otherwise the Authorization header is dropped and the API returns 401.
	apiKey := p.APIKey
	if strings.HasPrefix(apiKey, "$") {
		if v, err := a.Store().Resolve(apiKey); err == nil {
			apiKey = v
		}
	}
	baseURL := p.BaseURL
	if strings.HasPrefix(baseURL, "$") {
		if v, err := a.Store().Resolve(baseURL); err == nil {
			baseURL = v
		}
	}

	switch p.Type {
	case "cli":
		models := fetchModelsCLI()
		return models, warnings, nil
	case "openai-compat":
		if baseURL == "" {
			return nil, warnings, fmt.Errorf("openai-compat requires base_url")
		}
		models, warns, err := fetchModelsOpenAICompat(baseURL, apiKey)
		warnings = append(warnings, warns...)
		return models, warnings, err
	case "anthropic":
		if baseURL == "" {
			baseURL = "https://api.anthropic.com/v1"
		}
		models, warns, err := fetchModelsAnthropic(baseURL, apiKey)
		warnings = append(warnings, warns...)
		return models, warnings, err
	default:
		// Try catwalk-known
		if isCatwalkKnown(p.Type) {
			models, err := fetchModelsFromCatwalk(a, string(p.Type))
			return models, warnings, err
		}
		return nil, warnings, fmt.Errorf("unsupported provider type %q for model fetching", p.Type)
	}
}

// fetchModelsFromCatwalk gets models from the catwalk cache.
func fetchModelsFromCatwalk(a *app.App, providerType string) ([]catwalk.Model, error) {
	known := a.Store().KnownProviders()
	for _, kp := range known {
		if string(kp.ID) == providerType || strings.EqualFold(string(kp.ID), providerType) {
			return kp.Models, nil
		}
	}
	// Fallback: empty list
	return []catwalk.Model{}, nil
}

// fetchModelsOpenAICompat fetches from an OpenAI-compatible /models endpoint.
func fetchModelsOpenAICompat(baseURL, apiKey string) ([]catwalk.Model, []string, error) {
	var warnings []string
	baseURL = strings.TrimRight(baseURL, "/")
	endpoint := baseURL + "/models"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, warnings, fmt.Errorf("failed to create request: %w", err)
	}

	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, warnings, fmt.Errorf("failed to fetch models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, warnings, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextWindow int64  `json:"context_window,omitempty"`
		} `json:"data"`
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, warnings, fmt.Errorf("failed to parse response: %w", err)
	}

	models := make([]catwalk.Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		models = append(models, catwalk.Model{
			ID:            m.ID,
			Name:          m.ID,
			ContextWindow: m.ContextWindow,
		})
	}

	if len(models) > 0 {
		warnings = append(warnings, "Note: context window information is not available for openai-compat providers. "+
			"Set it manually with: crush providers set "+
			"<id> --json '{\"models\": [{\"id\": \"<model>\", \"context_window\": <tokens>}]}'")
	}

	return models, warnings, nil
}

// fetchModelsAnthropic fetches from Anthropic's /v1/models endpoint.
func fetchModelsAnthropic(baseURL, apiKey string) ([]catwalk.Model, []string, error) {
	var warnings []string
	baseURL = strings.TrimRight(baseURL, "/")
	endpoint := baseURL + "/models"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, warnings, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, warnings, fmt.Errorf("failed to fetch models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, warnings, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, warnings, fmt.Errorf("failed to parse response: %w", err)
	}

	models := make([]catwalk.Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		models = append(models, catwalk.Model{
			ID:   m.ID,
			Name: m.ID,
		})
	}

	return models, warnings, nil
}

// fetchModelsCLI fetches models from the hardcoded CLI providers.
func fetchModelsCLI() []catwalk.Model {
	specs := cliprovider.Available()
	models := make([]catwalk.Model, 0, len(specs))
	for _, spec := range specs {
		models = append(models, catwalk.Model{
			ID:   spec.ModelID,
			Name: spec.ModelName,
		})
	}
	return models
}

// isCatwalkKnown checks if the provider type is tracked by catwalk.
func isCatwalkKnown(t catwalk.Type) bool {
	known := []catwalk.Type{
		"openai", "anthropic", "gemini", "azure", "vertexai",
		"bedrock", "xai", "zai", "groq", "openrouter",
		"synthetic", "huggingface", "copilot", "vercel", "hyper",
	}
	for _, kt := range known {
		if t == kt {
			return true
		}
	}
	return false
}

// computeDiff computes added and removed models between two lists.
func computeDiff(old, new []catwalk.Model) (added, removed []catwalk.Model) {
	oldSet := make(map[string]bool)
	oldMap := make(map[string]catwalk.Model)
	for _, m := range old {
		oldSet[m.ID] = true
		oldMap[m.ID] = m
	}

	newSet := make(map[string]bool)
	newMap := make(map[string]catwalk.Model)
	for _, m := range new {
		newSet[m.ID] = true
		newMap[m.ID] = m
		if !oldSet[m.ID] {
			added = append(added, m)
		}
	}
	_ = newMap

	for id, m := range oldMap {
		if !newSet[id] {
			removed = append(removed, m)
		}
	}

	return added, removed
}
