package app

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShouldBypassSubAgentBan is the table-level test for the Фаза 2
// decision logic (docs/plans/2026-07-26-orchestrator-worker-e2e.md): the
// default `crush run` sub-agent ban is bypassed for the `agent` tool
// whenever --role resolved to smart/large AND a Worker model slot is
// configured with a non-empty Model — regardless of whether --agents
// single was passed explicitly or left unset, since a configured worker
// means delegation is the operator's intent either way.
func TestShouldBypassSubAgentBan(t *testing.T) {
	t.Parallel()

	workerConfigured := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeWorker: {Provider: "openai", Model: "gpt-4o-mini"},
		},
	}
	workerConfiguredEmptyModel := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			// Present but Model == "" must NOT count as configured.
			config.SelectedModelTypeWorker: {Provider: "openai", Model: ""},
		},
	}
	workerNotConfigured := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{},
	}

	tests := []struct {
		name string
		role config.SelectedModelType
		cfg  *config.Config
		want bool
	}{
		// --- The single most important case ---
		{
			name: "worker NOT configured + role smart => sub-agents still disabled, exactly as today",
			role: config.SelectedModelTypeLarge,
			cfg:  workerNotConfigured,
			want: false,
		},

		// --- worker configured x role ---
		{
			name: "worker configured, role smart => bypass (covers both --agents unset and explicit --agents single)",
			role: config.SelectedModelTypeLarge,
			cfg:  workerConfigured,
			want: true,
		},
		{
			name: "worker configured, role fast => no bypass (non-large role never bypasses)",
			role: config.SelectedModelTypeSmall,
			cfg:  workerConfigured,
			want: false,
		},

		// --- worker with empty Model (not really configured) ---
		{
			name: "worker present but Model empty, role smart => no bypass",
			role: config.SelectedModelTypeLarge,
			cfg:  workerConfiguredEmptyModel,
			want: false,
		},

		// --- nil config guard ---
		{
			name: "nil config never bypasses",
			role: config.SelectedModelTypeLarge,
			cfg:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shouldBypassSubAgentBan(tt.role, tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestShouldBypassSubAgentBan_BackwardCompatRequiresWorkerCondition proves
// that dropping the "worker configured" condition from the predicate
// would break the backward-compatibility guarantee: without it, a bare
// `crush run --role smart` (no worker configured at all — today's most
// common invocation) would incorrectly bypass the sub-agent ban. This
// test documents/pins that condition by re-deriving the role-only
// predicate inline and showing it disagrees with shouldBypassSubAgentBan
// on exactly the backward-compat case.
func TestShouldBypassSubAgentBan_BackwardCompatRequiresWorkerCondition(t *testing.T) {
	t.Parallel()

	workerNotConfigured := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{},
	}

	// Real predicate: worker not configured => never bypass.
	got := shouldBypassSubAgentBan(config.SelectedModelTypeLarge, workerNotConfigured)
	require.False(t, got, "worker not configured must never bypass the ban")

	// Predicate with the worker check dropped, i.e. only role checked,
	// mirroring what shouldBypassSubAgentBan would degrade to.
	withoutWorkerCheck := func(role config.SelectedModelType) bool {
		return role == config.SelectedModelTypeLarge
	}
	brokenResult := withoutWorkerCheck(config.SelectedModelTypeLarge)
	require.True(t, brokenResult,
		"dropping the worker check would incorrectly bypass the ban for a bare --role smart with no worker configured")
	require.NotEqual(t, got, brokenResult,
		"the real predicate and the worker-check-dropped predicate must disagree on the backward-compat case")
}

// TestDisableToolsInConfig_StripsExactlyNamedTools is the focused test on
// the refactored disableToolsInConfig: it must remove exactly the tool
// names passed in and leave everything else (including tools NOT in the
// removal list) untouched.
func TestDisableToolsInConfig_StripsExactlyNamedTools(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: map[string]config.Agent{
			config.AgentCoder: {
				ID:           config.AgentCoder,
				AllowedTools: []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"},
			},
		},
	}
	testApp := &App{config: config.NewTestStore(cfg)}

	testApp.disableToolsInConfig([]string{"agentic_fetch"})

	got := testApp.config.Config().Agents[config.AgentCoder].AllowedTools
	assert.Equal(t, []string{"view", "grep", "agent", "bash", "edit"}, got,
		"only agentic_fetch should be stripped; agent and everything else survives")
}

// TestDisableSubAgentToolsInConfig_StripsBothToolsUnchanged pins the
// unchanged default behaviour of disableSubAgentToolsInConfig (used by
// the non-bypass path, i.e. explicit --agents single or worker not
// configured): both "agent" and "agentic_fetch" are stripped.
func TestDisableSubAgentToolsInConfig_StripsBothToolsUnchanged(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: map[string]config.Agent{
			config.AgentCoder: {
				ID:           config.AgentCoder,
				AllowedTools: []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"},
			},
		},
	}
	testApp := &App{config: config.NewTestStore(cfg)}

	testApp.disableSubAgentToolsInConfig()

	got := testApp.config.Config().Agents[config.AgentCoder].AllowedTools
	assert.Equal(t, []string{"view", "grep", "bash", "edit"}, got)
	assert.NotContains(t, got, "agent")
	assert.NotContains(t, got, "agentic_fetch")
}
