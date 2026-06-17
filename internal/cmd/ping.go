package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/config"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/spf13/cobra"
)

var pingCmd = &cobra.Command{
	Use:   "ping [--json] [--timeout 15s] [--prompt \"<custom>\"]",
	Short: "Ping the large model to verify connectivity and API key",
	Long: `Send a minimal request to the configured large model to verify connectivity,
API key validity, and measure latency. Works with any provider: API-based
(Anthropic, OpenAI, Google, …) and CLI-based (claude, gemini, codex, qwen).

A system prompt instructs the model to reply with exactly "OK". Any other
response sets status=degraded (for CLI models, any non-empty reply counts
as ok because the local CLI injects its own system context).

Auth/quota errors set status=error with exit code 1.
Timeouts set status=timeout with exit code 2.`,
	Example: `
# Ping whichever large model is currently configured
crush ping

# Ping the small model slot
crush ping-fast

# First set the model, then ping (API provider)
crush models use glm5_turbo glm5_turbo && crush ping

# Set a CLI model and ping it (Claude via local CLI)
crush models use fh hh && crush ping

# Set Opus 4.8 via short code and ping
crush models use ox hl && crush ping --timeout 60s

# Machine-readable JSON
crush ping --json

# Custom prompt and timeout
crush ping --timeout 30s --prompt "Reply with yes or no"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPing(cmd, config.SelectedModelTypeLarge)
	},
}

var pingFastCmd = &cobra.Command{
	Use:   "ping-fast [--json] [--timeout 15s] [--prompt \"<custom>\"]",
	Short: "Ping the small model to verify connectivity and API key",
	Long: `Same as 'crush ping' but for the configured small (fast) model slot.
Works with any provider type — API or CLI.`,
	Example: `
# Ping the small model
crush ping-fast

# CLI model in the small slot
crush models use oh hl && crush ping-fast

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
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Effort           string  `json:"effort,omitempty"`
	Atom             string  `json:"atom,omitempty"`
	Status           string  `json:"status"`
	LatencyMs        int64   `json:"latency_ms"`
	Response         string  `json:"response,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	Error            *string `json:"error"`
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

	cwd, _ := ResolveCwd(cmd)

	// Build the provider
	provider, err := buildPingProvider(ctx, store, providerCfg, &modelCfg, cwd)
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

	// Pass reasoning effort to CLI providers via context.
	if modelCfg.ReasoningEffort != "" {
		ctx = context.WithValue(ctx, cliprovider.ReasoningEffortContextKey, modelCfg.ReasoningEffort)
	}

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
			printPingTextError(result, time.Time{})
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
		resetAt, _ := pingRateLimitReset(err, time.Now())
		printPingTextError(result, resetAt)
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
		printPingTextError(result, time.Time{})
		os.Exit(1)
		return nil
	}

	response := resp.Response.Content.Text()

	// Get tokens from usage
	promptTokens := resp.Steps[len(resp.Steps)-1].Usage.InputTokens
	completionTokens := resp.Steps[len(resp.Steps)-1].Usage.OutputTokens

	// Determine status based on response.
	// CLI providers pipe through `claude` which injects its own system prompt
	// (CLAUDE.md, project context, etc.), so the model rarely echoes bare "OK".
	// For CLI we accept any non-empty reply as healthy.
	status := "ok"
	if response != "OK" {
		if providerCfg.Type == cliprovider.ProviderType {
			if strings.TrimSpace(response) == "" {
				status = "degraded"
			}
		} else {
			status = "degraded"
		}
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
func buildPingProvider(ctx context.Context, store *config.ConfigStore, providerCfg config.ProviderConfig, modelCfg *config.SelectedModel, cwd string) (fantasy.Provider, error) {
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

	case cliprovider.ProviderType:
		return cliprovider.New(cwd, func() bool { return true }, nil, nil, nil), nil

	default:
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

// printPingTextError renders a failed ping (timeout / stream error / empty
// response) for the human-readable output path. Without this the text mode
// silently exited non-zero — a rate-limit / quota error from the provider
// produced no message at all, the error was only ever emitted under --json.
// Goes to stderr so it doesn't pollute any stdout parsing of the OK path.
// When resetAt is non-zero (rate-limit error), the moment the limit window
// reopens is printed in local time so the user knows when to retry.
func printPingTextError(r PingResult, resetAt time.Time) {
	if r.Effort != "" {
		fmt.Fprintf(os.Stderr, "provider:  %s / %s effort=%s\n", r.Provider, r.Model, r.Effort)
	} else {
		fmt.Fprintf(os.Stderr, "provider:  %s / %s\n", r.Provider, r.Model)
	}
	fmt.Fprintf(os.Stderr, "status:    %s\n", r.Status)
	fmt.Fprintf(os.Stderr, "latency:   %dms\n", r.LatencyMs)
	if r.Error != nil {
		fmt.Fprintf(os.Stderr, "error:     %s\n", *r.Error)
	}
	if !resetAt.IsZero() {
		local := resetAt.Local()
		if in := time.Until(local).Round(time.Second); in > 0 {
			fmt.Fprintf(os.Stderr, "limit reset: %s (in %s)\n", local.Format("2006-01-02 15:04:05 -07:00"), formatResetDuration(in))
		} else {
			fmt.Fprintf(os.Stderr, "limit reset: %s\n", local.Format("2006-01-02 15:04:05 -07:00"))
		}
	}
}

// formatResetDuration humanises a positive countdown — "4h12m07s" reads
// cleaner than Go's default "4h12m6.832s" because we round to seconds and
// omit zero leading components.
func formatResetDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// pingRateLimitReset extracts, from a provider error, the wall-clock time at
// which the rate-limit window reopens. The hint lives in the 429 response
// headers (never the error string), so it's read off fantasy.ProviderError's
// captured headers. Returns ok=false when the error isn't a provider error or
// carries no usable reset hint. `now` is taken as an argument for testability.
func pingRateLimitReset(err error, now time.Time) (time.Time, bool) {
	// Last-resort string parse: even when fantasy didn't surface a
	// ProviderError, the err.Error() text often still contains the
	// z.ai-style "Your limit will reset at ..." hint (it's wrapped by
	// the retry layer). Try it before we give up entirely.
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) {
		if t, ok := parseZAIResetHint(err.Error()); ok {
			return t, true
		}
		return time.Time{}, false
	}
	if pe.ResponseHeaders == nil {
		if pe.Message != "" {
			if t, ok := parseZAIResetHint(pe.Message); ok {
				return t, true
			}
		}
		if t, ok := parseZAIResetHint(err.Error()); ok {
			return t, true
		}
		return time.Time{}, false
	}
	get := func(name string) string {
		for k, v := range pe.ResponseHeaders {
			if strings.EqualFold(k, name) {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}

	// Retry-After (RFC 7231): delta-seconds or an HTTP-date.
	if ra := get("retry-after"); ra != "" {
		if secs, e := strconv.Atoi(ra); e == nil {
			return now.Add(time.Duration(secs) * time.Second), true
		}
		if t, e := http.ParseTime(ra); e == nil {
			return t, true
		}
	}

	// Anthropic reset headers: RFC 3339 timestamps (unix seconds for the
	// unified one). Take the latest — that's when every bucket has refilled.
	var latest time.Time
	for _, h := range []string{
		"anthropic-ratelimit-unified-reset",
		"anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-reset",
		"anthropic-ratelimit-requests-reset",
	} {
		v := get(h)
		if v == "" {
			continue
		}
		if t, e := time.Parse(time.RFC3339, v); e == nil {
			if t.After(latest) {
				latest = t
			}
			continue
		}
		if secs, e := strconv.ParseInt(v, 10, 64); e == nil {
			if t := time.Unix(secs, 0); t.After(latest) {
				latest = t
			}
		}
	}
	if !latest.IsZero() {
		return latest, true
	}

	// OpenAI-style: durations like "1s" / "6m0s" relative to now.
	var maxDur time.Duration
	for _, h := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if v := get(h); v != "" {
			if d, e := time.ParseDuration(v); e == nil && d > maxDur {
				maxDur = d
			}
		}
	}
	if maxDur > 0 {
		return now.Add(maxDur), true
	}

	// z.ai-style fallback: the reset hint only lives in the error body,
	// e.g. "Your limit will reset at 2026-06-17 14:49:28". No tz marker —
	// z.ai's servers report in China Standard Time (UTC+8), so we parse
	// the wall-clock as CST and let the caller .Local() it.
	if pe.Message != "" {
		if t, ok := parseZAIResetHint(pe.Message); ok {
			return t, true
		}
	}

	return time.Time{}, false
}

// zaiResetRe matches z.ai's "Your limit will reset at YYYY-MM-DD HH:MM:SS"
// fragment as emitted by their rate-limit error bodies. Time is captured
// without a zone — by convention CST (UTC+8).
var zaiResetRe = regexp.MustCompile(`limit will reset at\s+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)

func parseZAIResetHint(text string) (time.Time, bool) {
	m := zaiResetRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return time.Time{}, false
	}
	stamp := strings.ReplaceAll(m[1], "T", " ")
	// Fixed CST zone — z.ai's API surface is anchored there. Using a fixed
	// offset (no historical DST table) is exactly right for a wall-clock
	// stamp like this one.
	cst := time.FixedZone("CST", 8*3600)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, cst)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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
