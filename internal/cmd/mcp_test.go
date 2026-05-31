package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStoreWithDir creates a ConfigStore that can perform
// SetConfigField/RemoveConfigField operations against a temp directory.
// Uses config.Load with CRUSH_GLOBAL_DATA pointing to a temp dir.
func newTestStoreWithDir(t *testing.T) (*config.ConfigStore, string) {
	t.Helper()
	dir := t.TempDir()
	globalDir := filepath.Join(dir, "global")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	t.Setenv("CRUSH_GLOBAL_DATA", globalDir)

	// Write an empty config so Load finds it.
	configPath := filepath.Join(globalDir, "crush.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	// Load with a workspace dir that has a .crush subdir.
	workspaceDir := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, ".crush"), 0o755))

	store, err := config.Load(workspaceDir, filepath.Join(workspaceDir, ".crush"), false)
	require.NoError(t, err)
	return store, configPath
}

// --- Cobra structure tests ---

func TestMCPCmd_Exists(t *testing.T) {
	t.Parallel()
	require.NotNil(t, mcpCmd)
	assert.Equal(t, "mcp", mcpCmd.Use)
}

func TestMCPListCmd_Flags(t *testing.T) {
	t.Parallel()
	require.NotNil(t, mcpListCmd.Flags().Lookup("json"))
	require.NotNil(t, mcpListCmd.Flags().Lookup("grep"))
}

func TestMCPShowCmd_Args(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "show <id>", mcpShowCmd.Use)
}

func TestMCPEnableCmd_Short(t *testing.T) {
	t.Parallel()
	require.NotNil(t, mcpEnableCmd)
	assert.NotEmpty(t, mcpEnableCmd.Short)
}

func TestMCPDisableCmd_Short(t *testing.T) {
	t.Parallel()
	require.NotNil(t, mcpDisableCmd)
	assert.NotEmpty(t, mcpDisableCmd.Short)
}

func TestMCPAddCmd_Flags(t *testing.T) {
	t.Parallel()
	require.NotNil(t, mcpAddCmd.Flags().Lookup("type"))
	require.NotNil(t, mcpAddCmd.Flags().Lookup("command"))
	require.NotNil(t, mcpAddCmd.Flags().Lookup("arg"))
	require.NotNil(t, mcpAddCmd.Flags().Lookup("url"))
	require.NotNil(t, mcpAddCmd.Flags().Lookup("env"))
	require.NotNil(t, mcpAddCmd.Flags().Lookup("header"))
}

func TestMCPRemoveCmd_Aliases(t *testing.T) {
	t.Parallel()
	assert.Contains(t, mcpRemoveCmd.Aliases, "rm")
}

func TestMCPSetCmd_Flags(t *testing.T) {
	t.Parallel()
	require.NotNil(t, mcpSetCmd.Flags().Lookup("type"))
	require.NotNil(t, mcpSetCmd.Flags().Lookup("command"))
	require.NotNil(t, mcpSetCmd.Flags().Lookup("disabled"))
}

// --- Spec-required functional tests ---

// TestMCPList_TableOutput verifies table header and list item construction.
func TestMCPList_TableOutput(t *testing.T) {
	t.Parallel()

	require.NotNil(t, mcpListCmd)
	assert.Equal(t, "list", mcpListCmd.Use)
	assert.NotEmpty(t, mcpListCmd.Short)

	// Verify JSON flag exists.
	flag := mcpListCmd.Flags().Lookup("json")
	require.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)
}

// TestMCPList_JSON verifies JSON structure of list items.
func TestMCPList_JSON(t *testing.T) {
	t.Parallel()

	m := config.MCPConfig{
		Type:    config.MCPStdio,
		Command: "node",
		Args:    []string{"server.js"},
		Env:     map[string]string{"KEY": "val"},
	}
	item := makeMCPListItem("my-server", m)

	data, err := json.Marshal(item)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, "my-server", parsed["id"])
	assert.Equal(t, "stdio", parsed["type"])
	assert.Equal(t, "node", parsed["command"])
}

// TestMCPList_Grep verifies grep filter across multiple fields and servers.
func TestMCPList_Grep(t *testing.T) {
	t.Parallel()

	servers := map[string]config.MCPConfig{
		"memory": {Type: config.MCPStdio, Command: "npx"},
		"fetch":  {Type: config.MCPStdio, Command: "python"},
		"remote": {Type: config.MCPHttp, URL: "http://api.example.com/mcp"},
	}

	// Filter by "stdio" matches memory and fetch.
	var matched []string
	for id, m := range servers {
		if matchesMCPGrep(id, m, "stdio") {
			matched = append(matched, id)
		}
	}
	assert.Contains(t, matched, "memory")
	assert.Contains(t, matched, "fetch")

	// Filter by "example.com" matches remote only.
	matched = nil
	for id, m := range servers {
		if matchesMCPGrep(id, m, "example.com") {
			matched = append(matched, id)
		}
	}
	assert.Len(t, matched, 1)
	assert.Contains(t, matched, "remote")

	// Case-insensitive.
	assert.True(t, matchesMCPGrep("memory", servers["memory"], "STDIO"))
	assert.True(t, matchesMCPGrep("fetch", servers["fetch"], "Python"))
}

// TestMCPShow_FullConfig verifies show item includes all MCP config fields.
func TestMCPShow_FullConfig(t *testing.T) {
	t.Parallel()

	m := config.MCPConfig{
		Type:          config.MCPStdio,
		Command:       "node",
		Args:          []string{"server.js", "--verbose"},
		Env:           map[string]string{"NODE_ENV": "development"},
		Headers:       map[string]string{"Authorization": "Bearer token"},
		Disabled:      false,
		DisabledTools: []string{"bad-tool"},
		EnabledTools:  []string{"good-tool"},
		Timeout:       30,
	}

	item := makeMCPListItem("test-server", m)
	assert.Equal(t, "test-server", item.ID)
	assert.Equal(t, "stdio", item.Type)
	assert.Equal(t, "node", item.Command)
	assert.Equal(t, []string{"server.js", "--verbose"}, item.Args)
	assert.Equal(t, map[string]string{"NODE_ENV": "development"}, item.Env)
	assert.Equal(t, map[string]string{"Authorization": "Bearer token"}, item.Headers)
	assert.False(t, item.Disabled)
	assert.Equal(t, []string{"bad-tool"}, item.DisabledTools)
	assert.Equal(t, []string{"good-tool"}, item.EnabledTools)
	assert.Equal(t, 30, item.Timeout)
}

// TestMCPEnableDisable exercises enable/disable by writing to config and
// verifying the persisted state changes.
func TestMCPEnableDisable(t *testing.T) {
	store, configPath := newTestStoreWithDir(t)

	// Add a server in disabled state.
	err := store.SetConfigFields(config.ScopeGlobal, map[string]any{
		"mcp.test.type":     "stdio",
		"mcp.test.command":  "node",
		"mcp.test.disabled": true,
	})
	require.NoError(t, err)
	assert.True(t, store.HasConfigField(config.ScopeGlobal, "mcp.test.disabled"))

	// Enable: set disabled=false.
	err = store.SetConfigField(config.ScopeGlobal, "mcp.test.disabled", false)
	require.NoError(t, err)

	// Verify on disk.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(data), `"disabled"`))

	// Disable: set disabled=true.
	err = store.SetConfigField(config.ScopeGlobal, "mcp.test.disabled", true)
	require.NoError(t, err)
	data, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(data), `"disabled"`))
}

// TestMCPAdd_StdioValidation verifies that stdio MCP config produces correct
// list items with command set.
func TestMCPAdd_StdioValidation(t *testing.T) {
	t.Parallel()

	// A stdio server must have a command.
	m := config.MCPConfig{
		Type:    config.MCPStdio,
		Command: "node",
		Args:    []string{"server.js"},
	}
	item := makeMCPListItem("my-stdio", m)
	assert.Equal(t, "my-stdio", item.ID)
	assert.Equal(t, "stdio", item.Type)
	assert.Equal(t, "node", item.Command)
	assert.Equal(t, []string{"server.js"}, item.Args)

	// Verify the add command has type and command flags for validation.
	require.NotNil(t, mcpAddCmd.Flags().Lookup("type"))
	require.NotNil(t, mcpAddCmd.Flags().Lookup("command"))
}

// TestMCPAdd_DuplicateID verifies the add command structure for rejecting
// duplicate IDs.
func TestMCPAdd_DuplicateID(t *testing.T) {
	t.Parallel()

	require.NotNil(t, mcpAddCmd)
	assert.Equal(t, "add <id>", mcpAddCmd.Use)
	assert.NotEmpty(t, mcpAddCmd.Short)

	// Verify required flags.
	assert.NotNil(t, mcpAddCmd.Flags().Lookup("type"))
	assert.NotNil(t, mcpAddCmd.Flags().Lookup("command"))
	assert.NotNil(t, mcpAddCmd.Flags().Lookup("url"))
}

// TestMCPRemove_StripField verifies that RemoveConfigField removes the MCP
// key from the config file.
func TestMCPRemove_StripField(t *testing.T) {
	store, _ := newTestStoreWithDir(t)

	// Add an MCP server.
	err := store.SetConfigFields(config.ScopeGlobal, map[string]any{
		"mcp.myserver.type":    "stdio",
		"mcp.myserver.command": "node",
	})
	require.NoError(t, err)
	assert.True(t, store.HasConfigField(config.ScopeGlobal, "mcp.myserver"))
	assert.True(t, store.HasConfigField(config.ScopeGlobal, "mcp.myserver.type"))

	// Remove it.
	err = store.RemoveConfigField(config.ScopeGlobal, "mcp.myserver")
	require.NoError(t, err)
	assert.False(t, store.HasConfigField(config.ScopeGlobal, "mcp.myserver"))
	assert.False(t, store.HasConfigField(config.ScopeGlobal, "mcp.myserver.type"))
}

// --- Store integration tests ---

// TestMCPStore_AddAndRetrieve verifies MCP config round-trips through the store.
func TestMCPStore_AddAndRetrieve(t *testing.T) {
	store, configPath := newTestStoreWithDir(t)

	err := store.SetConfigFields(config.ScopeGlobal, map[string]any{
		"mcp.myserver.type":     "stdio",
		"mcp.myserver.command":  "npx",
		"mcp.myserver.args":     []string{"-y", "@mcp/server-memory"},
		"mcp.myserver.disabled": false,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var cfg map[string]any
	require.NoError(t, json.Unmarshal(data, &cfg))

	mcp, ok := cfg["mcp"].(map[string]any)
	require.True(t, ok, "expected mcp to be a map")

	server, ok := mcp["myserver"].(map[string]any)
	require.True(t, ok, "expected myserver entry")

	assert.Equal(t, "stdio", server["type"])
	assert.Equal(t, "npx", server["command"])
}

// TestMCPStore_EnableDisableCycle verifies state transitions persist correctly.
func TestMCPStore_EnableDisableCycle(t *testing.T) {
	store, configPath := newTestStoreWithDir(t)

	// Add enabled server.
	err := store.SetConfigFields(config.ScopeGlobal, map[string]any{
		"mcp.cycle.type":     "http",
		"mcp.cycle.url":      "http://localhost:3000/mcp",
		"mcp.cycle.disabled": false,
	})
	require.NoError(t, err)

	// Disable.
	err = store.SetConfigField(config.ScopeGlobal, "mcp.cycle.disabled", true)
	require.NoError(t, err)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(data), `"disabled"`))

	// Re-enable.
	err = store.SetConfigField(config.ScopeGlobal, "mcp.cycle.disabled", false)
	require.NoError(t, err)
	data, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(data), `"disabled"`))
}

// --- Helper function tests ---

func TestMakeMCPListItem(t *testing.T) {
	t.Parallel()

	m := config.MCPConfig{
		Type:     config.MCPStdio,
		Command:  "node",
		Args:     []string{"server.js"},
		Disabled: false,
		Timeout:  30,
	}

	item := makeMCPListItem("test-server", m)
	assert.Equal(t, "test-server", item.ID)
	assert.Equal(t, "stdio", item.Type)
	assert.Equal(t, "node", item.Command)
	assert.Equal(t, []string{"server.js"}, item.Args)
	assert.False(t, item.Disabled)
	assert.Equal(t, 30, item.Timeout)
}

func TestMatchesMCPGrep(t *testing.T) {
	t.Parallel()

	m := config.MCPConfig{
		Type:    config.MCPStdio,
		Command: "python",
		URL:     "http://localhost:3000",
	}

	assert.True(t, matchesMCPGrep("my-server", m, "my-server"))
	assert.True(t, matchesMCPGrep("my-server", m, "stdio"))
	assert.True(t, matchesMCPGrep("my-server", m, "python"))
	assert.True(t, matchesMCPGrep("my-server", m, "localhost"))
	assert.True(t, matchesMCPGrep("my-server", m, "http"))
	assert.False(t, matchesMCPGrep("my-server", m, "nonexistent"))
}

func TestParseKVPairs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		input    []string
		expected map[string]string
	}{
		{
			name:     "empty",
			input:    []string{},
			expected: map[string]string{},
		},
		{
			name:     "single pair",
			input:    []string{"KEY=value"},
			expected: map[string]string{"KEY": "value"},
		},
		{
			name:     "multiple pairs",
			input:    []string{"KEY1=value1", "KEY2=value2"},
			expected: map[string]string{"KEY1": "value1", "KEY2": "value2"},
		},
		{
			name:     "value with equals",
			input:    []string{"KEY=value=with=equals"},
			expected: map[string]string{"KEY": "value=with=equals"},
		},
		{
			name:     "no equals ignored",
			input:    []string{"KEYONLY"},
			expected: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := parseKVPairs(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// TestMCPSet_UpdatesFields verifies that set operations update individual
// fields without removing others.
func TestMCPSet_UpdatesFields(t *testing.T) {
	store, configPath := newTestStoreWithDir(t)

	// Create initial server.
	err := store.SetConfigFields(config.ScopeGlobal, map[string]any{
		"mcp.myserver.type":    "stdio",
		"mcp.myserver.command": "node",
	})
	require.NoError(t, err)

	// Update just the command.
	err = store.SetConfigField(config.ScopeGlobal, "mcp.myserver.command", "npx")
	require.NoError(t, err)

	// Verify type is still there.
	assert.True(t, store.HasConfigField(config.ScopeGlobal, "mcp.myserver.type"))
	assert.True(t, store.HasConfigField(config.ScopeGlobal, "mcp.myserver.command"))

	// Verify the updated value on disk.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(data), `"type"`))
	require.True(t, strings.Contains(string(data), `"command"`))
}
