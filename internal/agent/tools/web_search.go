package tools

import (
	"context"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"charm.land/fantasy"
)

//go:embed web_search.md.tpl
var webSearchDescriptionTmpl []byte

var webSearchDescriptionTpl = template.Must(
	template.New("webSearchDescription").
		Parse(string(webSearchDescriptionTmpl)),
)

// NewWebSearchTool creates a web search tool for sub-agents (no permissions needed).
func NewWebSearchTool(client *http.Client) fantasy.AgentTool {
	if client == nil {
		// SSRF-guarded for consistency with the other model-facing HTTP
		// tools — see ssrf_guard.go. The search endpoint itself is fixed,
		// not model-controlled, so this is defense-in-depth rather than
		// closing a direct exfiltration path.
		client = NewSSRFGuardedClient(30*time.Second, false)
	}

	return fantasy.NewParallelAgentTool(
		WebSearchToolName,
		renderToolDescription(webSearchDescriptionTpl),
		func(ctx context.Context, params WebSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Query == "" {
				return fantasy.NewTextErrorResponse("query is required"), nil
			}

			maxResults := params.MaxResults
			if maxResults <= 0 {
				maxResults = 10
			}
			if maxResults > 20 {
				maxResults = 20
			}

			if err := maybeDelaySearch(ctx); err != nil {
				return fantasy.NewTextErrorResponse("Search cancelled: " + err.Error()), nil
			}
			results, err := searchDuckDuckGo(ctx, client, params.Query, maxResults)
			slog.Debug("Web search completed", "query", params.Query, "results", len(results), "err", err)
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to search: " + err.Error()), nil
			}

			return fantasy.NewTextResponse(formatSearchResults(results)), nil
		},
	)
}
