package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/charmbracelet/crush/internal/projects"
	"github.com/stretchr/testify/require"
)

// isolateGlobalConfigDir points GlobalConfig() (CRUSH_GLOBAL_CONFIG/
// XDG_CONFIG_HOME) at a throwaway directory distinct from whatever
// XDG_DATA_HOME/CRUSH_GLOBAL_DATA the caller already isolated.
// projects.List/Register here only ever touch config.GlobalConfigData() (to
// find the sibling projects.json), never config.Load/GlobalConfig(), so
// there's no actual host-config leak risk on this path today — this is
// added purely for defense-in-depth/consistency with the other CLI test
// harnesses in this package that DO call config.Load (see mcp_test.go's
// newTestStoreWithDir and models_use_test.go's isolatedModelsEnv).
func isolateGlobalConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", dir)
}

func TestProjectsEmpty(t *testing.T) {
	// Use a temp directory for projects.json
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	isolateGlobalConfigDir(t)

	var b bytes.Buffer
	projectsCmd.SetOut(&b)
	projectsCmd.SetErr(&b)
	projectsCmd.SetIn(bytes.NewReader(nil))
	err := projectsCmd.RunE(projectsCmd, nil)
	require.NoError(t, err)
	require.Equal(t, "No projects tracked yet.\n", b.String())
}

func TestProjectsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	isolateGlobalConfigDir(t)

	// Register a project
	err := projects.Register("/test/project", "/test/project/.crush")
	require.NoError(t, err)

	var b bytes.Buffer
	projectsCmd.SetOut(&b)
	projectsCmd.SetErr(&b)
	projectsCmd.SetIn(bytes.NewReader(nil))

	// Set the --json flag
	projectsCmd.Flags().Set("json", "true")
	defer projectsCmd.Flags().Set("json", "false")

	err = projectsCmd.RunE(projectsCmd, nil)
	require.NoError(t, err)

	// Parse the JSON output
	var result struct {
		Projects []projects.Project `json:"projects"`
	}
	err = json.Unmarshal(b.Bytes(), &result)
	require.NoError(t, err)

	require.Len(t, result.Projects, 1)
	require.Equal(t, "/test/project", result.Projects[0].Path)
	require.Equal(t, "/test/project/.crush", result.Projects[0].DataDir)
}
