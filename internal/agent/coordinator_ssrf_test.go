package agent

// TestBuildTools_AllowPrivateNetworkFetchEscapeHatch is the regression test
// for B-1, found by an independent @oh review of the CS-8 SSRF guard: the
// guard's own doc-comments and CHANGELOG entry claimed "callers that
// legitimately need loopback pass their own client built with
// NewSSRFGuardedClient(timeout, true)" — but no such caller existed in
// production wiring, so a routine local-dev fetch (e.g.
// fetch("http://localhost:3000/api/health")) was permanently blocked with
// no way to restore it short of a code change. Options.AllowPrivateNetworkFetch
// closes that gap: this test proves the coordinator's buildTools actually
// wires it through to the download tool end-to-end.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func findTool(t *testing.T, built []fantasy.AgentTool, name string) fantasy.AgentTool {
	t.Helper()
	for _, tool := range built {
		if tool.Info().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found among built tools", name)
	return nil
}

func TestBuildTools_AllowPrivateNetworkFetchEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "local dev server response")
	}))
	t.Cleanup(srv.Close)

	runDownload := func(t *testing.T, coord *coordinator) (fantasy.ToolResponse, error) {
		t.Helper()
		coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
		require.True(t, ok, "coder agent must be configured")
		built, err := coord.buildTools(t.Context(), coderCfg, false)
		require.NoError(t, err)

		downloadTool := findTool(t, built, tools.DownloadToolName)
		input, err := json.Marshal(tools.DownloadParams{URL: srv.URL, FilePath: "local.bin"})
		require.NoError(t, err)

		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "test-session")
		return downloadTool.Run(ctx, fantasy.ToolCall{ID: "test-call", Name: tools.DownloadToolName, Input: string(input)})
	}

	t.Run("option off default, loopback download is SSRF-blocked", func(t *testing.T) {
		env := testEnv(t)
		coord := newWorkerToolTestCoordinator(t, env, false)
		require.False(t, coord.cfg.Config().Options.AllowPrivateNetworkFetch, "must default to false")

		_, err := runDownload(t, coord)
		require.Error(t, err, "download to a loopback httptest.Server must be blocked by default")
	})

	t.Run("option on, loopback download succeeds", func(t *testing.T) {
		env := testEnv(t)
		coord := newWorkerToolTestCoordinator(t, env, false)
		coord.cfg.Config().Options.AllowPrivateNetworkFetch = true

		resp, err := runDownload(t, coord)
		require.NoError(t, err, "with the escape hatch enabled, a loopback download must succeed")
		require.False(t, resp.IsError, "response: %s", resp.Content)
	})
}
