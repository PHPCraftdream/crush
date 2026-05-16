package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var providersTestCmd = &cobra.Command{
	Use:   "test <id>",
	Short: "Ping a provider's API to verify the configured credentials and base URL",
	Long: `Make a small read-only request against the provider's API to find
out whether crush would be able to talk to it. Reports the HTTP status,
a short error message on failure, and how many models the endpoint
returned on success.

The exact endpoint depends on the provider type:
  - openai / openai-compat / hyper / local-cli : GET <base_url>/models
  - anthropic                                  : GET https://api.anthropic.com/v1/models
                                                 (uses x-api-key header)
  - gemini                                     : GET <base_url>/models?key=<api_key>
  - everything else (azure, vertex…)           : GET <base_url>/models, best effort

No tokens are spent — these are catalog endpoints, not completions.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush providers test openai
crush providers test ollama   # works with a local openai-compat server too
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		timeout, _ := cmd.Flags().GetDuration("timeout")
		if timeout <= 0 {
			timeout = 15 * time.Second
		}

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		p, ok := a.Config().Providers.Get(args[0])
		if !ok {
			return fmt.Errorf("provider %q not configured", args[0])
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()

		result := pingProvider(ctx, args[0], p)
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		fmt.Fprintf(os.Stdout, "id:        %s\n", result.ID)
		fmt.Fprintf(os.Stdout, "endpoint:  %s\n", result.Endpoint)
		fmt.Fprintf(os.Stdout, "status:    %s\n", result.Status)
		if result.HTTPCode != 0 {
			fmt.Fprintf(os.Stdout, "http:      %d\n", result.HTTPCode)
		}
		if result.ModelsFound > 0 {
			fmt.Fprintf(os.Stdout, "models:    %d returned\n", result.ModelsFound)
		}
		if result.Error != "" {
			fmt.Fprintf(os.Stdout, "error:     %s\n", result.Error)
		}
		fmt.Fprintf(os.Stdout, "latency:   %dms\n", result.LatencyMs)
		// Non-zero exit for failures so wrapper scripts can branch on it.
		if !result.OK {
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	providersTestCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")
	providersTestCmd.Flags().Duration("timeout", 0, "HTTP timeout (default 15s)")
	providersCmd.AddCommand(providersTestCmd)
}

type providerPingResult struct {
	ID          string `json:"id"`
	Endpoint    string `json:"endpoint"`
	OK          bool   `json:"ok"`
	Status      string `json:"status"` // "ok", "unauthorized", "not_found", "unreachable", "no_api_key", "error"
	HTTPCode    int    `json:"http_code,omitempty"`
	ModelsFound int    `json:"models_found,omitempty"`
	Error       string `json:"error,omitempty"`
	LatencyMs   int64  `json:"latency_ms"`
}

func pingProvider(ctx context.Context, id string, p config.ProviderConfig) providerPingResult {
	res := providerPingResult{ID: id}

	if p.APIKey == "" && p.OAuthToken == nil && !strings.EqualFold(string(p.Type), "cli") {
		// CLI providers are local processes — no API key needed. Everything
		// else basically requires credentials to reach catalogs.
		res.Status = "no_api_key"
		res.Error = "provider has no api_key (and no oauth token) configured"
		return res
	}

	baseURL := strings.TrimRight(p.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURLFor(p)
	}
	endpoint := baseURL + "/models"
	if strings.EqualFold(string(p.Type), "gemini") {
		// Gemini wants the key as a query param.
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		endpoint = endpoint + sep + "key=" + p.APIKey
	}
	res.Endpoint = endpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		res.Status = "error"
		res.Error = err.Error()
		return res
	}

	switch strings.ToLower(string(p.Type)) {
	case "anthropic":
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		// Key already in URL; no header needed.
	default:
		if p.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
	}
	for k, v := range p.ExtraHeaders {
		if v == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Status = "unreachable"
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.HTTPCode = resp.StatusCode

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		res.Status = "unauthorized"
		res.Error = "credentials rejected"
		return res
	case resp.StatusCode == http.StatusNotFound:
		res.Status = "not_found"
		res.Error = "no /models endpoint at this base URL (provider may not be OpenAI-compatible)"
		return res
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		res.Status = "error"
		res.Error = strings.TrimSpace(string(body))
		return res
	}

	// Try to count models in the response payload. Best-effort: not every
	// provider returns the same shape. Failure here doesn't downgrade OK.
	res.OK = true
	res.Status = "ok"
	if body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)); len(body) > 0 {
		var generic struct {
			Data   []json.RawMessage `json:"data"`
			Models []json.RawMessage `json:"models"`
		}
		if json.Unmarshal(body, &generic) == nil {
			res.ModelsFound = len(generic.Data) + len(generic.Models)
		}
	}
	return res
}

func defaultBaseURLFor(p config.ProviderConfig) string {
	switch strings.ToLower(string(p.Type)) {
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta"
	default:
		return "https://api.openai.com/v1"
	}
}
