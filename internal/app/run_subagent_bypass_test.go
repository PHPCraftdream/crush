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
// ONLY when --agents was left unset, --role resolved to smart/large, and
// a Worker model slot is configured with a non-empty Model.
//
// Matrix: {agents unset, agents single (explicit), agents agent-allow,
// agents with-agents} x {worker configured, not configured} x {role
// smart, role fast}. Only "agents unset" is a real DisableSubAgents=true
// case in run.go (agent-allow/with-agents never set DisableSubAgents at
// all), but shouldBypassSubAgentBan is exercised across the whole
// explicit/role/worker cube anyway since it's a pure function and the
// other combinations pin "explicit always wins" / "role must be large"
// regardless of how DisableSubAgents was derived upstream.
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
		name     string
		explicit bool // true == operator passed --agents (e.g. "single") directly
		role     config.SelectedModelType
		cfg      *config.Config
		want     bool
	}{
		// --- The single most important case ---
		{
			name:     "worker NOT configured + role smart + flags unset => sub-agents still disabled, exactly as today",
			explicit: false,
			role:     config.SelectedModelTypeLarge,
			cfg:      workerNotConfigured,
			want:     false,
		},

		// --- agents unset (implicit default) x worker configured x role ---
		{
			name:     "agents unset, worker configured, role smart => bypass",
			explicit: false,
			role:     config.SelectedModelTypeLarge,
			cfg:      workerConfigured,
			want:     true,
		},
		{
			name:     "agents unset, worker configured, role fast => no bypass (non-large role never bypasses)",
			explicit: false,
			role:     config.SelectedModelTypeSmall,
			cfg:      workerConfigured,
			want:     false,
		},

		// --- agents unset x worker with empty Model (not really configured) ---
		{
			name:     "agents unset, worker present but Model empty, role smart => no bypass",
			explicit: false,
			role:     config.SelectedModelTypeLarge,
			cfg:      workerConfiguredEmptyModel,
			want:     false,
		},

		// --- explicit --agents single always wins, regardless of worker/role ---
		{
			name:     "explicit --agents single, worker configured, role smart => explicit wins, no bypass",
			explicit: true,
			role:     config.SelectedModelTypeLarge,
			cfg:      workerConfigured,
			want:     false,
		},
		{
			name:     "explicit --agents single, worker configured, role fast => explicit wins, no bypass",
			explicit: true,
			role:     config.SelectedModelTypeSmall,
			cfg:      workerConfigured,
			want:     false,
		},
		{
			name:     "explicit --agents single, worker NOT configured, role smart => explicit wins, no bypass",
			explicit: true,
			role:     config.SelectedModelTypeLarge,
			cfg:      workerNotConfigured,
			want:     false,
		},
		{
			name:     "explicit --agents single, worker NOT configured, role fast => explicit wins, no bypass",
			explicit: true,
			role:     config.SelectedModelTypeSmall,
			cfg:      workerNotConfigured,
			want:     false,
		},

		// --- nil config guard ---
		{
			name:     "nil config never bypasses",
			explicit: false,
			role:     config.SelectedModelTypeLarge,
			cfg:      nil,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shouldBypassSubAgentBan(tt.explicit, tt.role, tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestShouldBypassSubAgentBan_BackwardCompatRequiresWorkerCondition proves
// that dropping the "worker configured" condition (c) from the predicate
// would break the backward-compatibility guarantee: without it, a bare
// `crush run --role smart` (no worker configured at all — today's most
// common invocation) would incorrectly bypass the sub-agent ban. This
// test documents/pins condition (c) by re-deriving the two-condition
// (explicit, role-only) predicate inline and showing it disagrees with
// shouldBypassSubAgentBan on exactly the backward-compat case.
func TestShouldBypassSubAgentBan_BackwardCompatRequiresWorkerCondition(t *testing.T) {
	t.Parallel()

	workerNotConfigured := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{},
	}

	// Real predicate: worker not configured => never bypass.
	got := shouldBypassSubAgentBan(false, config.SelectedModelTypeLarge, workerNotConfigured)
	require.False(t, got, "worker not configured must never bypass the ban")

	// Predicate with condition (c) dropped, i.e. only (explicit, role)
	// checked, mirroring what shouldBypassSubAgentBan would degrade to.
	withoutWorkerCheck := func(explicit bool, role config.SelectedModelType) bool {
		if explicit {
			return false
		}
		return role == config.SelectedModelTypeLarge
	}
	brokenResult := withoutWorkerCheck(false, config.SelectedModelTypeLarge)
	require.True(t, brokenResult,
		"dropping condition (c) would incorrectly bypass the ban for a bare --role smart with no worker configured")
	require.NotEqual(t, got, brokenResult,
		"the real predicate and the condition-(c)-dropped predicate must disagree on the backward-compat case")
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
