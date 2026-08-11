package tools

// Regression test for BL-1, found by an independent @oh review of the CS-8
// SSRF guard: web_fetch (and agentic_fetch, web_search, sourcegraph, which
// share the same "if client == nil" fallback pattern) built their own
// unguarded default http.Client instead of reusing download/fetch's guarded
// one — web_fetch in particular takes a model-controlled URL and explicitly
// skips the permission system ("no permissions needed"), so it was a
// complete bypass of the SSRF protection CS-8 was supposed to add.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestWebFetchTool_SSRFBlockedOnLoopback(t *testing.T) {
	t.Parallel()

	const secret = "iam/security-credentials/role-name"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, secret)
	}))
	t.Cleanup(srv.Close)

	// nil client -> SSRF-guarded default, matching production wiring
	// (coordinator.go's agenticFetchTool passes this same client through
	// to NewWebFetchTool).
	tool := NewWebFetchTool(t.TempDir(), nil)

	input, err := json.Marshal(WebFetchParams{URL: srv.URL})
	require.NoError(t, err)

	resp, runErr := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "test-call",
		Name:  WebFetchToolName,
		Input: string(input),
	})
	require.NoError(t, runErr, "web_fetch reports failures via ToolResponse.IsError, not a Go error")
	require.True(t, resp.IsError, "guarded web_fetch must refuse a loopback URL")
	require.NotContains(t, resp.Content, secret, "blocked web_fetch must not leak body text")
}

func TestWebFetchTool_HappyPathWithAllowPrivate(t *testing.T) {
	t.Parallel()

	const body = "hello from a local page"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>"+body+"</body></html>")
	}))
	t.Cleanup(srv.Close)

	tool := NewWebFetchTool(t.TempDir(), NewSSRFGuardedClient(5*time.Second, true))

	input, err := json.Marshal(WebFetchParams{URL: srv.URL})
	require.NoError(t, err)

	resp, runErr := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "test-call",
		Name:  WebFetchToolName,
		Input: string(input),
	})
	require.NoError(t, runErr)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, body)
}
