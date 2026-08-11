package agent

// P350 regression test (found via a CI-only failure that never reproduced
// locally, 2026-08-10): config.Load/reload only call Config.SetupAgents
// (which populates cfg.Agents, including AgentCoder — required by both
// NewCoordinator here and App.InitCoderAgent in internal/app) when
// IsConfigured() is already true AT THE MOMENT they run. Several tests
// across this codebase (TestP1_3_CoordinatorSummarizeSingleRead,
// TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot,
// internal/app's p348_p0_1_* tests, and likely others) call config.Init
// against a genuinely empty config, then mutate cfg.Config().Providers
// directly afterward — bypassing config.Load/reload's own pipeline
// entirely, so SetupAgents never runs and cfg.Agents[AgentCoder] stays
// empty even though IsConfigured() has since become true.
//
// This was masked on the orchestrator's own dev machine: identified by the
// sixth @oh review pass — internal/config/load.go's cliprovider.Available()
// synthesizes a local-cli provider whenever claude/gemini/codex/qwen is on
// PATH, making IsConfigured() already true at the moment config.Init runs
// (regardless of CRUSH_GLOBAL_CONFIG/CRUSH_GLOBAL_DATA/XDG_CONFIG_HOME/
// XDG_DATA_HOME isolation, since PATH detection isn't file-based), so
// SetupAgents fires during that initial call and the gap never surfaces
// locally on a machine with any of those CLIs installed. It reproduces
// reliably on a genuinely clean environment (GitHub Actions CI, all three OS
// runners) as "coder agent not configured" / "coder agent configuration is
// missing".
//
// Fixed at the root, in NewCoordinator (here) and App.InitCoderAgent
// (internal/app/app.go): both now self-heal by calling cfg.SetupAgents()
// and re-checking when the initial Agents[AgentCoder] lookup misses.
// SetupAgents is idempotent (derives Agents purely from
// Options/DisabledTools, no I/O), so this is safe regardless of why the gap
// occurred. This closes the whole class of gap at the two places every
// caller (test or production) funnels through, rather than requiring every
// test that mutates Providers directly to also remember an explicit
// SetupAgents call.
//
// This test does not depend on any ambient environment leak: it forces the
// exact precondition directly (IsConfigured() true, Agents explicitly
// cleared) so it fails deterministically without the fix, regardless of
// what machine or CI runner it executes on.
//
// REVERT CHECK PROCEDURE:
//  1. In coordinator.go's NewCoordinator, remove the self-heal block (the
//     `if !ok { cfg.SetupAgents(); ... }` right before
//     `errCoderAgentNotConfigured` is returned).
//  2. Run: go test ./internal/agent -run TestReleaseGate_P350_NewCoordinatorSelfHealsMissingAgentsMap -v
//  3. FAIL: NewCoordinator returns errCoderAgentNotConfigured.
//  4. Restore the self-heal block and PASS.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestReleaseGate_P350_NewCoordinatorSelfHealsMissingAgentsMap(t *testing.T) {
	env := testEnv(t)

	// Server is never actually called — NewCoordinator only needs to
	// construct a language-model client (buildAgentModels), not stream
	// from it, but a syntactically valid, non-empty BaseURL is required.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("provider must not be called — this test only exercises coordinator construction")
	}))
	t.Cleanup(srv.Close)

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	// NewCoordinator's buildAgentModels needs a selected large/small model
	// to construct the coordinator at all, independently of the
	// Agents[AgentCoder] self-heal this test targets — found via a CI-only
	// failure ("large model not selected") that never reproduced locally.
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeLarge, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	cfg.SetSelectedModelRuntime(config.SelectedModelTypeSmall, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	require.True(t, cfg.Config().IsConfigured(), "a provider must be configured for this test to be meaningful")

	// Force the exact precondition this fix targets, regardless of whatever
	// made IsConfigured() true above: Agents explicitly empty, simulating
	// SetupAgents never having run.
	cfg.Config().Agents = map[string]config.Agent{}
	require.Empty(t, cfg.Config().Agents[config.AgentCoder].ID, "precondition: Agents[AgentCoder] must be genuinely missing before NewCoordinator runs")

	_, err = NewCoordinator(t.Context(), cfg, env.sessions, env.messages, env.permissions, env.history, *env.filetracker, nil)
	require.NoError(t, err, "NewCoordinator must self-heal by calling SetupAgents when Agents[AgentCoder] is missing but a provider is configured")
}
