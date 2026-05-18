package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/config"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/spf13/cobra"
)

var pingCmd = &cobra.Command{
	Use:   "ping [--json] [--timeout 15s] [--prompt \"<custom>\"]",
	Short: "Ping the large model to verify connectivity and API key",
	Long: `Send a minimal request to the configured large model to verify connectivity,
API key validity, and measure latency. This is a per-slot ping (complements
'crush providers test <id>' which is per-provider).

A system prompt instructs the model to reply with exactly "OK". Any other
response sets status=degraded. Auth/quota errors set status=error with exit
code 1. Timeouts set status=timeout with exit code 2.`,
	Example: `
# Default text output
crush ping

# Machine-readable JSON
crush ping --json

# With 30s timeout
crush ping --timeout 30s

# Custom prompt
crush ping --prompt "Reply with yes or no"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPing(cmd, config.SelectedModelTypeLarge)
	},
}

var pingFastCmd = &cobra.Command{
	Use:   "ping-fast [--json] [--timeout 15s] [--prompt \"<custom>\"]",
	Short: "Ping the small model to verify connectivity and API key",
	Long: `Same as 'crush ping' but for the configured small model.`,
	Example: `
# Default text output
crush ping-fast

# Machine-readable JSON
crush ping-fast --json

# With 30s timeout
crush ping-fast --timeout 30s
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPing(cmd, config.SelectedModelTypeSmall)
	},
}

type PingResult struct {
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	Effort             string `json:"effort,omitempty"`
	Atom               string `json:"atom,omitempty"`
	Status             string `json:"status"`
	LatencyMs          int64  `json:"latency_ms"`
	Response           string `json:"response,omitempty"`
	PromptTokens       int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens   int64  `json:"completion_tokens,omitempty"`
	CostUSD            float64 `json:"cost_usd,omitempty"`
	Error              *string `json:"error"`
}

func runPing(cmd *cobra.Command, modelType config.SelectedModelType) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	customPrompt, _ := cmd.Flags().GetString("prompt")

	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	cfg := a.Config()
	store := a.Store()

	// Get effective model
	modelCfg, ok := cfg.Models[modelType]
	if !ok {
		msg := fmt.Sprintf("%s model not configured", modelType)
		result := PingResult{
			Status: "error",
			Error:  &msg,
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		return fmt.Errorf("%s", msg)
	}

	// Get provider config
	providerCfg, ok := cfg.Providers.Get(modelCfg.Provider)
	if !ok {
		msg := fmt.Sprintf("provider %q not found", modelCfg.Provider)
		result := PingResult{
			Provider: modelCfg.Provider,
			Model:    modelCfg.Model,
			Status:   "error",
			Error:    &msg,
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		return fmt.Errorf("%s", msg)
	}

	// Prepare context with timeout
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	// Build the provider
	provider, err := buildPingProvider(ctx, store, providerCfg, &modelCfg)
	if err != nil {
		msg := err.Error()
		result := PingResult{
			Provider: modelCfg.Provider,
			Model:    modelCfg.Model,
			Status:   "error",
			Error:    &msg,
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		return err
	}

	// Get language model from provider
	langModel, err := provider.LanguageModel(ctx, modelCfg.Model)
	if err != nil {
		msg := err.Error()
		result := PingResult{
			Provider: modelCfg.Provider,
			Model:    modelCfg.Model,
			Status:   "error",
			Error:    &msg,
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		return err
	}

	// Set up the ping request
	userPrompt := "ping"
	if customPrompt != "" {
		userPrompt = customPrompt
	}

	systemPrompt := "You are a network liveness probe. Reply with exactly the word OK and nothing else."

	// Create agent
	agent := fantasy.NewAgent(
		langModel,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithMaxOutputTokens(1024),
	)

	// Measure request time
	start := time.Now()

	// Stream the response
	streamCall := fantasy.AgentStreamCall{
		Prompt: userPrompt,
	}

	resp, err := agent.Stream(ctx, streamCall)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result := PingResult{
				Provider:  modelCfg.Provider,
				Model:     modelCfg.Model,
				Effort:    modelCfg.ReasoningEffort,
				Status:    "timeout",
				LatencyMs: latency,
				Error:     stringPtr("request timeout"),
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			os.Exit(2)
			return nil
		}

		msg := err.Error()
		result := PingResult{
			Provider:  modelCfg.Provider,
			Model:     modelCfg.Model,
			Effort:    modelCfg.ReasoningEffort,
			Status:    "error",
			LatencyMs: latency,
			Error:     &msg,
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		os.Exit(1)
		return nil
	}

	if resp == nil {
		msg := "no response from model"
		result := PingResult{
			Provider:  modelCfg.Provider,
			Model:     modelCfg.Model,
			Effort:    modelCfg.ReasoningEffort,
			Status:    "error",
			LatencyMs: latency,
			Error:     &msg,
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		os.Exit(1)
		return nil
	}

	response := resp.Response.Content.Text()

	// Get tokens from usage
	promptTokens := resp.Steps[len(resp.Steps)-1].Usage.InputTokens
	completionTokens := resp.Steps[len(resp.Steps)-1].Usage.OutputTokens

	// Determine status based on response
	status := "ok"
	if response != "OK" {
		status = "degraded"
	}

	// Build atom label
	atom := lookupAtomForModel(modelCfg)

	result := PingResult{
		Provider:         modelCfg.Provider,
		Model:            modelCfg.Model,
		Effort:           modelCfg.ReasoningEffort,
		Atom:             atom,
		Status:           status,
		LatencyMs:        latency,
		Response:         response,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CostUSD:          0, // Cost calculation requires model pricing config
		Error:            nil,
	}

	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	// Text output
	if atom != "" {
		if modelCfg.ReasoningEffort != "" {
			fmt.Printf("provider:  %s / %s effort=%s   (atom: %s-%s)\n",
				modelCfg.Provider, modelCfg.Model, modelCfg.ReasoningEffort, atom, modelCfg.ReasoningEffort)
		} else {
			fmt.Printf("provider:  %s / %s   (atom: %s)\n",
				modelCfg.Provider, modelCfg.Model, atom)
		}
	} else {
		if modelCfg.ReasoningEffort != "" {
			fmt.Printf("provider:  %s / %s effort=%s\n",
				modelCfg.Provider, modelCfg.Model, modelCfg.ReasoningEffort)
		} else {
			fmt.Printf("provider:  %s / %s\n", modelCfg.Provider, modelCfg.Model)
		}
	}
	fmt.Printf("status:    %s\n", status)
	fmt.Printf("latency:   %dms\n", latency)
	fmt.Printf("response:  %s\n", response)
	if promptTokens > 0 || completionTokens > 0 {
		fmt.Printf("tokens:    %d in, %d out\n", promptTokens, completionTokens)
	}

	// Exit codes
	switch status {
	case "ok":
		return nil
	case "degraded":
		os.Exit(3)
	default:
		os.Exit(1)
	}
	return nil
}

// buildPingProvider constructs a provider for the ping request.
// Mirrors the coordinator's buildProvider logic.
func buildPingProvider(ctx context.Context, store *config.ConfigStore, providerCfg config.ProviderConfig, modelCfg *config.SelectedModel) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// Handle special headers for anthropic thinking
	if providerCfg.Type == anthropic.Name && modelCfg.Think {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, _ := store.Resolve(providerCfg.APIKey)
	baseURL, _ := store.Resolve(providerCfg.BaseURL)

	switch providerCfg.Type {
	case openai.Name:
		opts := []openai.Option{
			openai.WithAPIKey(apiKey),
			openai.WithUseResponsesAPI(),
		}
		if len(headers) > 0 {
			opts = append(opts, openai.WithHeaders(headers))
		}
		if baseURL != "" {
			opts = append(opts, openai.WithBaseURL(baseURL))
		}
		return openai.New(opts...)

	case anthropic.Name:
		opts := []anthropic.Option{
			anthropic.WithAPIKey(apiKey),
		}
		if baseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(baseURL))
		}
		if len(headers) > 0 {
			opts = append(opts, anthropic.WithHeaders(headers))
		}
		return anthropic.New(opts...)

	case openrouter.Name:
		opts := []openrouter.Option{
			openrouter.WithAPIKey(apiKey),
		}
		if len(headers) > 0 {
			opts = append(opts, openrouter.WithHeaders(headers))
		}
		return openrouter.New(opts...)

	case vercel.Name:
		opts := []vercel.Option{
			vercel.WithAPIKey(apiKey),
		}
		if len(headers) > 0 {
			opts = append(opts, vercel.WithHeaders(headers))
		}
		return vercel.New(opts...)

	case azure.Name:
		opts := []azure.Option{
			azure.WithBaseURL(baseURL),
			azure.WithAPIKey(apiKey),
			azure.WithUseResponsesAPI(),
		}
		if providerCfg.ExtraParams != nil {
			if apiVersion, ok := providerCfg.ExtraParams["apiVersion"]; ok {
				opts = append(opts, azure.WithAPIVersion(apiVersion))
			}
		}
		if len(headers) > 0 {
			opts = append(opts, azure.WithHeaders(headers))
		}
		return azure.New(opts...)

	case bedrock.Name:
		var opts []bedrock.Option
		if apiKey != "" {
			opts = append(opts, bedrock.WithAPIKey(apiKey))
		}
		if len(headers) > 0 {
			opts = append(opts, bedrock.WithHeaders(headers))
		}
		return bedrock.New(opts...)

	case google.Name:
		opts := []google.Option{
			google.WithBaseURL(baseURL),
			google.WithGeminiAPIKey(apiKey),
		}
		if len(headers) > 0 {
			opts = append(opts, google.WithHeaders(headers))
		}
		return google.New(opts...)

	case "google-vertex":
		opts := []google.Option{}
		if len(headers) > 0 {
			opts = append(opts, google.WithHeaders(headers))
		}
		if providerCfg.ExtraParams != nil {
			project := providerCfg.ExtraParams["project"]
			location := providerCfg.ExtraParams["location"]
			opts = append(opts, google.WithVertex(project, location))
		}
		return google.New(opts...)

	case openaicompat.Name, hyper.Name:
		if providerCfg.ID == hyper.Name {
			baseURL = hyper.BaseURL() + "/v1"
		}
		opts := []openaicompat.Option{
			openaicompat.WithBaseURL(baseURL),
			openaicompat.WithAPIKey(apiKey),
		}
		if len(headers) > 0 {
			opts = append(opts, openaicompat.WithHeaders(headers))
		}
		if providerCfg.ExtraBody != nil {
			for extraKey, extraValue := range providerCfg.ExtraBody {
				opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
			}
		}
		return openaicompat.New(opts...)

	default:
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

func stringPtr(s string) *string {
	return &s
}

func init() {
	pingCmd.Flags().Bool("json", false, "Emit JSON output instead of human-readable text")
	pingCmd.Flags().Duration("timeout", 15*time.Second, "Request timeout")
	pingCmd.Flags().String("prompt", "", "Custom user prompt (default: \"ping\")")

	pingFastCmd.Flags().Bool("json", false, "Emit JSON output instead of human-readable text")
	pingFastCmd.Flags().Duration("timeout", 15*time.Second, "Request timeout")
	pingFastCmd.Flags().String("prompt", "", "Custom user prompt (default: \"ping\")")

	rootCmd.AddCommand(pingCmd)
	rootCmd.AddCommand(pingFastCmd)
}
