package permission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBashParams mirrors the shape of tools.BashPermissionsParams so the
// reflection-based extractor in the permission package can read the
// Command field without importing internal/agent/tools (which would be
// a cycle). Keep the field name in sync with BashPermissionsParams.
type fakeBashParams struct {
	Description string `json:"description"`
	Command     string `json:"command"`
	WorkingDir  string `json:"working_dir"`
}

// newRunTestService is newTestService + an auto-approved session, so the
// restricted-run gate is the only thing left to exercise.
func newRunTestService(t *testing.T) Service {
	t.Helper()
	svc := newTestService(t, false, nil)
	svc.AutoApproveSession("run-session")
	return svc
}

// TestRunGate_DefaultStillAutoApproves is the backwards-compatibility
// guarantee: with no restricted allowlist armed, `crush run` keeps its
// current behaviour of approving every permission request.
func TestRunGate_DefaultStillAutoApproves(t *testing.T) {
	svc := newRunTestService(t)

	// No SetRunAllowlist call => inert allowlist => legacy behaviour.
	result, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "run-session",
		ToolName:  "bash",
		Action:    "execute",
		Path:      "/tmp",
		Params:    fakeBashParams{Command: "rm -rf /"},
	})
	require.NoError(t, err)
	assert.True(t, result, "default crush run must still auto-approve even rm -rf /")
}

// TestRunGate_RestrictedDeniesWithoutWaiting is the core safety
// property: when restricted mode is armed and the request does not match
// the allowlist, Request must return false promptly. It must NOT block
// waiting on a UI that doesn't exist in non-interactive mode.
func TestRunGate_RestrictedDeniesWithoutWaiting(t *testing.T) {
	svc := newRunTestService(t)
	armed, err := BuildRunAllowlist(RunAllowlistSpec{Restrict: true})
	require.NoError(t, err)
	svc.SetRunAllowlist(armed)

	done := make(chan bool, 1)
	go func() {
		r, _ := svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "run-session",
			ToolName:  "bash",
			Action:    "execute",
			Path:      "/tmp",
			Params:    fakeBashParams{Command: "rm -rf /"},
		})
		done <- r
	}()

	select {
	case got := <-done:
		assert.False(t, got, "non-allowlisted command must be denied in restricted mode")
	case <-time.After(2 * time.Second):
		t.Fatal("restricted-mode deny blocked for >2s; Request hung waiting for a UI event")
	}
}

// TestRunGate_BashAllowlistPermitsMatching verifies that a bash command
// matching an allowlist pattern is approved, and a non-matching one is
// rejected — both in restricted mode.
func TestRunGate_BashAllowlistPermitsMatching(t *testing.T) {
	svc := newRunTestService(t)
	armed, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"git diff", "glob:ls *"},
	})
	require.NoError(t, err)
	svc.SetRunAllowlist(armed)

	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"matching prefix", "git diff HEAD~1", true},
		{"matching glob", "ls -la", true},
		{"non-matching", "rm -rf /", false},
		{"compound smuggling", "ls && rm -rf /", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.Request(t.Context(), CreatePermissionRequest{
				SessionID: "run-session",
				ToolName:  "bash",
				Action:    "execute",
				Path:      "/tmp",
				Params:    fakeBashParams{Command: tt.cmd},
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRunGate_NonBashToolAllowedByToolTable verifies that non-bash tools
// are governed by the AllowTools table (tool / tool:action entries).
func TestRunGate_NonBashToolAllowedByToolTable(t *testing.T) {
	svc := newRunTestService(t)
	armed, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"view", "edit:write"},
	})
	require.NoError(t, err)
	svc.SetRunAllowlist(armed)

	t.Run("view allowed", func(t *testing.T) {
		got, err := svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "run-session", ToolName: "view", Action: "read", Path: "/tmp",
		})
		require.NoError(t, err)
		assert.True(t, got)
	})
	t.Run("edit:write allowed", func(t *testing.T) {
		got, err := svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "run-session", ToolName: "edit", Action: "write", Path: "/tmp",
		})
		require.NoError(t, err)
		assert.True(t, got)
	})
	t.Run("bash denied without command match", func(t *testing.T) {
		got, err := svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "run-session", ToolName: "bash", Action: "execute", Path: "/tmp",
			Params: fakeBashParams{Command: "ls"},
		})
		require.NoError(t, err)
		assert.False(t, got, "bash not in allow_tools and no allow_bash => deny")
	})
	t.Run("random tool denied", func(t *testing.T) {
		got, err := svc.Request(t.Context(), CreatePermissionRequest{
			SessionID: "run-session", ToolName: "download", Action: "fetch", Path: "/tmp",
		})
		require.NoError(t, err)
		assert.False(t, got)
	})
}

// TestRunGate_ConfigAndCLIMerge exercises the documented merge: CLI
// patterns union with config patterns, and --restrict-run forces
// restrict on even when config has it off.
func TestRunGate_ConfigAndCLIMerge(t *testing.T) {
	// "Config" spec: restrict OFF, allows only git diff.
	configSpec := RunAllowlistSpec{
		Restrict:  false,
		AllowBash: []string{"git diff"},
	}
	// "CLI" override: --restrict-run + --allow-bash 'ls'.
	cliSpec := RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"ls"},
	}
	merged := MergeRunAllowlistSpecs(configSpec, cliSpec)
	require.True(t, merged.Restrict, "CLI --restrict-run must arm the gate")

	compiled, err := BuildRunAllowlist(merged)
	require.NoError(t, err)

	svc := newRunTestService(t)
	svc.SetRunAllowlist(compiled)

	// Both config and CLI patterns are honoured.
	got, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "run-session", ToolName: "bash", Action: "execute", Path: "/tmp",
		Params: fakeBashParams{Command: "git diff HEAD"},
	})
	require.NoError(t, err)
	assert.True(t, got, "config allow_bash pattern must be honoured")

	got, err = svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "run-session", ToolName: "bash", Action: "execute", Path: "/tmp",
		Params: fakeBashParams{Command: "ls -la"},
	})
	require.NoError(t, err)
	assert.True(t, got, "CLI allow_bash pattern must be honoured")

	got, err = svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "run-session", ToolName: "bash", Action: "execute", Path: "/tmp",
		Params: fakeBashParams{Command: "rm -rf /"},
	})
	require.NoError(t, err)
	assert.False(t, got, "non-allowlisted command denied after merge")
}

// TestRunGate_GlobalAllowedToolsStillBypasses confirms that the existing
// permissions.allowed_tools fast-path (checked before the gate) is
// preserved: if bash is listed there, it is allowed unconditionally even
// in restricted mode. This is the documented precedence — users who
// want command-level control must NOT list bash in allowed_tools.
func TestRunGate_GlobalAllowedToolsStillBypasses(t *testing.T) {
	svc := newTestService(t, false, []string{"bash"})
	svc.AutoApproveSession("run-session")
	armed, err := BuildRunAllowlist(RunAllowlistSpec{Restrict: true})
	require.NoError(t, err)
	svc.SetRunAllowlist(armed)

	got, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "run-session", ToolName: "bash", Action: "execute", Path: "/tmp",
		Params: fakeBashParams{Command: "rm -rf /"},
	})
	require.NoError(t, err)
	assert.True(t, got, "allowed_tools is a global bypass that still wins over the run gate")
}

// TestRunGate_CanBeDisarmed verifies SetRunAllowlist is idempotent and
// that passing a non-restricted allowlist restores legacy behaviour.
func TestRunGate_CanBeDisarmed(t *testing.T) {
	svc := newRunTestService(t)
	armed, _ := BuildRunAllowlist(RunAllowlistSpec{Restrict: true})
	svc.SetRunAllowlist(armed)

	// First deny to confirm armed.
	got, _ := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "run-session", ToolName: "bash", Action: "execute", Path: "/tmp",
		Params: fakeBashParams{Command: "rm -rf /"},
	})
	require.False(t, got)

	// Disarm => legacy auto-approve restored.
	inert, _ := BuildRunAllowlist(RunAllowlistSpec{Restrict: false})
	svc.SetRunAllowlist(inert)
	got, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "run-session", ToolName: "bash", Action: "execute", Path: "/tmp",
		Params: fakeBashParams{Command: "rm -rf /"},
	})
	require.NoError(t, err)
	assert.True(t, got, "disarmed gate restores auto-approve")
}
