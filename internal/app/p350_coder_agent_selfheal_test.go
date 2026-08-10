package app

// P350 regression test — App level companion to
// internal/agent/p350_coder_agent_selfheal_test.go. See that file's doc
// comment for the full root-cause explanation (config.Load/reload only
// call Config.SetupAgents when IsConfigured() is already true at the
// moment they run; a caller that mutates Providers/SelectedModel directly
// on an already-published config bypasses that entirely, leaving
// cfg.Agents[AgentCoder] empty even once a provider is configured).
//
// This test targets App.InitCoderAgent's own self-heal fallback
// specifically (internal/app/app.go), forcing the exact precondition
// directly rather than depending on any ambient environment leak, so it
// fails deterministically without the fix regardless of machine or CI
// runner.
//
// REVERT CHECK PROCEDURE:
//  1. In app.go's InitCoderAgent, remove the self-heal block (the
//     `if coderAgentCfg.ID == "" { app.config.SetupAgents(); ... }` before
//     the final "coder agent configuration is missing" error return).
//  2. Run: go test ./internal/app -run TestReleaseGate_P350_InitCoderAgentSelfHealsMissingAgentsMap -v
//  3. FAIL: InitCoderAgent returns "coder agent configuration is missing".
//  4. Restore the self-heal block and PASS.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestReleaseGate_P350_InitCoderAgentSelfHealsMissingAgentsMap(t *testing.T) {
	// Same isolation rationale as p348_p0_1_pump_coordinator_wiring_test.go
	// — see that file's comment for the full explanation.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("CRUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeLarge, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmall, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	require.True(t, store.Config().IsConfigured(), "a provider must be configured for this test to be meaningful")

	// Force the exact precondition this fix targets, regardless of whatever
	// made IsConfigured() true above: Agents explicitly empty, simulating
	// SetupAgents never having run.
	store.Config().Agents = map[string]config.Agent{}
	require.Empty(t, store.Config().Agents[config.AgentCoder].ID, "precondition: Agents[AgentCoder] must be genuinely missing before app.New runs")

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err, "App.New (via InitCoderAgent) must self-heal by calling SetupAgents when Agents[AgentCoder] is missing but a provider is configured")
	t.Cleanup(func() {
		if application.RunQueuePump != nil {
			application.RunQueuePump.Stop()
		}
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	require.NotNil(t, application.AgentCoordinator, "InitCoderAgent must have run and assigned a coordinator")
}
