package app

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunAllowlist_BashToolNameLiteralMatchesConstant pins the string
// literal that internal/permission/runallowlist.go hardcodes ("bash", in
// allowsRequest) to the canonical tools.BashToolName. The permission
// package can't import internal/agent/tools (import cycle), so it matches
// the literal directly; this cross-package assertion — in a package that
// CAN see both — turns a future rename of the bash tool into a failing
// test instead of a silent bypass, where the renamed tool would fall
// through to toolAllowed and a "bash" entry in run.allow_tools could
// start authorising arbitrary shell commands.
func TestRunAllowlist_BashToolNameLiteralMatchesConstant(t *testing.T) {
	t.Parallel()
	require.Equal(t, "bash", tools.BashToolName,
		`internal/permission/runallowlist.go hardcodes "bash" in allowsRequest; `+
			`if the bash tool is renamed, update that literal (and this guard) too`)
}

// TestRunAllowlistSpecFromConfig_NilPreservesLegacy is the backwards-
// compatibility guarantee: with no permissions.run block at all, the
// derived spec is inert (Restrict = false) so `crush run` keeps
// auto-approving everything.
func TestRunAllowlistSpecFromConfig_NilPreservesLegacy(t *testing.T) {
	t.Parallel()

	t.Run("nil permissions", func(t *testing.T) {
		t.Parallel()
		spec := runAllowlistSpecFromConfig(nil)
		assert.False(t, spec.Restrict, "nil Permissions => inert spec")
		assert.Empty(t, spec.AllowTools)
		assert.Empty(t, spec.AllowBash)
	})
	t.Run("nil run block", func(t *testing.T) {
		t.Parallel()
		spec := runAllowlistSpecFromConfig(&config.Permissions{})
		assert.False(t, spec.Restrict, "nil Run => inert spec")
		assert.Empty(t, spec.AllowTools)
		assert.Empty(t, spec.AllowBash)
	})
	t.Run("restrict false", func(t *testing.T) {
		t.Parallel()
		spec := runAllowlistSpecFromConfig(&config.Permissions{
			Run: &config.RunPermissions{
				Restrict:   false,
				AllowBash:  []string{"git diff"},
				AllowTools: []string{"view"},
			},
		})
		// Lists are still copied (so a CLI --restrict-run override can
		// arm them without re-reading config), but Restrict stays off.
		assert.False(t, spec.Restrict)
		assert.Equal(t, []string{"view"}, spec.AllowTools)
		assert.Equal(t, []string{"git diff"}, spec.AllowBash)
	})
}

// TestRunAllowlistSpecFromConfig_CopiesLists verifies the spec holds
// defensive copies of the config slices so a later config reload can't
// mutate an already-built allowlist.
func TestRunAllowlistSpecFromConfig_CopiesLists(t *testing.T) {
	t.Parallel()
	src := &config.Permissions{
		Run: &config.RunPermissions{
			Restrict:   true,
			AllowTools: []string{"view"},
			AllowBash:  []string{"git diff"},
		},
	}
	spec := runAllowlistSpecFromConfig(src)

	// Mutate the config after building the spec.
	src.Run.AllowTools = append(src.Run.AllowTools, "edit")
	src.Run.AllowBash = append(src.Run.AllowBash, "rm")
	src.Run.Restrict = false

	assert.Equal(t, []string{"view"}, spec.AllowTools, "spec slice is a copy, not an alias")
	assert.Equal(t, []string{"git diff"}, spec.AllowBash, "spec slice is a copy, not an alias")
	assert.True(t, spec.Restrict, "spec Restrict was captured before the mutation")
}

// TestRunAllowlistSpecFromConfig_FullConfig builds the spec from a fully
// populated config block and verifies it compiles into a working gate.
func TestRunAllowlistSpecFromConfig_FullConfig(t *testing.T) {
	t.Parallel()
	spec := runAllowlistSpecFromConfig(&config.Permissions{
		Run: &config.RunPermissions{
			Restrict:   true,
			AllowTools: []string{"view", "edit:write"},
			AllowBash:  []string{"git diff", "glob:ls *"},
		},
	})
	assert.True(t, spec.Restrict)
	assert.Equal(t, []string{"view", "edit:write"}, spec.AllowTools)
	assert.Equal(t, []string{"git diff", "glob:ls *"}, spec.AllowBash)

	// The compiled gate must actually work — this catches wiring bugs
	// where the spec is correct but BuildRunAllowlist disagrees.
	compiled, err := permission.BuildRunAllowlist(spec)
	require.NoError(t, err)
	require.True(t, compiled.IsRestricted())

	// Non-bash allow_tools entry works.
	spec2 := runAllowlistSpecFromConfig(&config.Permissions{
		Run: &config.RunPermissions{
			Restrict:   true,
			AllowTools: []string{"view"},
		},
	})
	compiled2, err := permission.BuildRunAllowlist(spec2)
	require.NoError(t, err)
	assert.True(t, requestAllowed(t, compiled2, permission.CreatePermissionRequest{
		ToolName: "view", Action: "read",
	}))
}

// TestRunOverridesMerge_ConfigPlusCLI is the unit-level test for the
// merge that RunNonInteractive performs: CLI flags union with config,
// and --restrict-run forces restrict on even when config has it off.
// It replicates the exact merge code from RunNonInteractive so a
// regression there is caught here without spinning up a full App.
func TestRunOverridesMerge_ConfigPlusCLI(t *testing.T) {
	t.Parallel()

	// "Config" spec: restrict OFF, allows only git diff via bash and
	// view via tools.
	configPerms := &config.Permissions{
		Run: &config.RunPermissions{
			Restrict:   false,
			AllowBash:  []string{"git diff"},
			AllowTools: []string{"view"},
		},
	}
	// "CLI" override: --restrict-run + --allow-bash 'ls' + --allow-tool edit.
	overrides := RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"ls"},
		AllowTools:    []string{"edit"},
	}

	// Replicate the merge from RunNonInteractive.
	runSpec := runAllowlistSpecFromConfig(configPerms)
	if overrides.RestrictedRun {
		runSpec.Restrict = true
	}
	runSpec.AllowBash = append(runSpec.AllowBash, overrides.AllowBash...)
	runSpec.AllowTools = append(runSpec.AllowTools, overrides.AllowTools...)

	require.True(t, runSpec.Restrict, "CLI --restrict-run forces restrict on")
	compiled, err := permission.BuildRunAllowlist(runSpec)
	require.NoError(t, err)
	require.True(t, compiled.IsRestricted())

	// Config bash pattern survives.
	type p struct{ Command string }
	assert.True(t, requestAllowed(t, compiled, permission.CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: p{Command: "git diff HEAD~1"},
	}), "config allow_bash pattern honoured after merge")
	// CLI bash pattern survives.
	assert.True(t, requestAllowed(t, compiled, permission.CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: p{Command: "ls -la"},
	}), "CLI allow_bash pattern honoured after merge")
	// Non-matching bash command denied.
	assert.False(t, requestAllowed(t, compiled, permission.CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: p{Command: "rm -rf /"},
	}))
	// Config + CLI non-bash tool entries both survive.
	assert.True(t, requestAllowed(t, compiled, permission.CreatePermissionRequest{
		ToolName: "view", Action: "read",
	}))
	assert.True(t, requestAllowed(t, compiled, permission.CreatePermissionRequest{
		ToolName: "edit", Action: "",
	}))
}

// TestRunOverridesMerge_CLIRestrictOffKeepsConfigBehaviour verifies
// that when neither CLI nor config arms restrict, the merged gate stays
// inert — i.e. legacy `crush run` auto-approve is preserved.
func TestRunOverridesMerge_CLIRestrictOffKeepsConfigBehaviour(t *testing.T) {
	t.Parallel()
	configPerms := &config.Permissions{
		Run: &config.RunPermissions{Restrict: false},
	}
	overrides := RunOverrides{RestrictedRun: false}

	runSpec := runAllowlistSpecFromConfig(configPerms)
	if overrides.RestrictedRun {
		runSpec.Restrict = true
	}
	compiled, _ := permission.BuildRunAllowlist(runSpec)
	assert.False(t, compiled.IsRestricted(), "no restrict from either side => inert gate")
}

// TestRunOverridesMerge_ConfigBashEntryInAllowToolsIsIgnored is the
// app-level mirror of the conservative bash guarantee: even if the
// config lists "bash" in run.allow_tools, the merged gate must NOT
// authorise bash commands without an allow_bash match. This catches a
// regression where the config helper accidentally undoes the
// gate-level guard.
func TestRunOverridesMerge_ConfigBashEntryInAllowToolsIsIgnored(t *testing.T) {
	t.Parallel()
	configPerms := &config.Permissions{
		Run: &config.RunPermissions{
			Restrict:   true,
			AllowTools: []string{"bash", "bash:execute"}, // must be ignored
			// No AllowBash => every bash command denied.
		},
	}
	spec := runAllowlistSpecFromConfig(configPerms)
	compiled, err := permission.BuildRunAllowlist(spec)
	require.NoError(t, err)

	type p struct{ Command string }
	denied := requestAllowed(t, compiled, permission.CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: p{Command: "ls"},
	})
	assert.False(t, denied, "allow_tools with bash/bash:execute must not bypass the gate")
}

func requestAllowed(t *testing.T, allowlist permission.RunAllowlist, req permission.CreatePermissionRequest) bool {
	t.Helper()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	const sessionID = "run-session"
	if req.SessionID == "" {
		req.SessionID = sessionID
	}
	if req.Path == "" {
		req.Path = "/tmp"
	}

	svc := permission.NewPermissionService(t.Context(), "/tmp", false, nil, db.New(conn))
	svc.AutoApproveSession(sessionID)
	svc.SetRunAllowlist(allowlist)

	allowed, err := svc.Request(t.Context(), req)
	require.NoError(t, err)
	return allowed
}
