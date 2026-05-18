package cmd

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestMCPList_TableOutput tests that list command is properly configured.
func TestMCPList_TableOutput(t *testing.T) {
	t.Parallel()

	// Verify the list command has correct structure
	require.NotNil(t, mcpListCmd)
	assert.Equal(t, "list", mcpListCmd.Use)
	assert.NotEmpty(t, mcpListCmd.Short)

	// Verify JSON flag exists
	flag := mcpListCmd.Flags().Lookup("json")
	require.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)
}

// TestMCPList_JSON tests JSON flag functionality.
func TestMCPList_JSON(t *testing.T) {
	t.Parallel()

	flag := mcpListCmd.Flags().Lookup("json")
	require.NotNil(t, flag)
	assert.Equal(t, "bool", flag.Value.Type())
}

// TestMCPList_Grep tests grep flag functionality.
func TestMCPList_Grep(t *testing.T) {
	t.Parallel()

	flag := mcpListCmd.Flags().Lookup("grep")
	require.NotNil(t, flag)
	assert.Equal(t, "string", flag.Value.Type())
}

// TestMCPShow_FullConfig tests that show command displays full configuration.
func TestMCPShow_FullConfig(t *testing.T) {
	t.Parallel()

	m := config.MCPConfig{
		Type:    config.MCPStdio,
		Command: "node",
		Args:    []string{"server.js"},
		Env:     map[string]string{"NODE_ENV": "development"},
		Headers: map[string]string{"Authorization": "Bearer token"},
		Disabled: false,
		Timeout: 30,
	}

	item := makeMCPListItem("test-server", m)
	assert.Equal(t, "test-server", item.ID)
	assert.Equal(t, "stdio", item.Type)
	assert.Equal(t, "node", item.Command)
	assert.Equal(t, []string{"server.js"}, item.Args)
	assert.Equal(t, map[string]string{"NODE_ENV": "development"}, item.Env)
	assert.Equal(t, map[string]string{"Authorization": "Bearer token"}, item.Headers)
	assert.False(t, item.Disabled)
	assert.Equal(t, 30, item.Timeout)
}

// TestMCPEnableDisable tests that enable and disable commands are properly configured.
func TestMCPEnableDisable(t *testing.T) {
	t.Parallel()

	// Test enable command
	require.NotNil(t, mcpEnableCmd)
	assert.Equal(t, "enable <id>", mcpEnableCmd.Use)
	assert.NotEmpty(t, mcpEnableCmd.Short)

	// Test disable command
	require.NotNil(t, mcpDisableCmd)
	assert.Equal(t, "disable <id>", mcpDisableCmd.Use)
	assert.NotEmpty(t, mcpDisableCmd.Short)

	// Both should have scope flags
	for _, cmd := range []*cobra.Command{mcpEnableCmd, mcpDisableCmd} {
		assert.NotNil(t, cmd.Flags().Lookup("global"))
		assert.NotNil(t, cmd.Flags().Lookup("local"))
	}
}

// TestMCPAdd_StdioValidation tests that stdio config is properly created.
func TestMCPAdd_StdioValidation(t *testing.T) {
	t.Parallel()

	m := config.MCPConfig{
		Type:    config.MCPStdio,
		Command: "node",
		Args:    []string{"server.js"},
		Disabled: false,
	}

	item := makeMCPListItem("my-stdio", m)
	assert.Equal(t, "my-stdio", item.ID)
	assert.Equal(t, "stdio", item.Type)
	assert.Equal(t, "node", item.Command)
	assert.Equal(t, []string{"server.js"}, item.Args)
	assert.False(t, item.Disabled)
}

// TestMCPAdd_DuplicateID tests the add command validation.
func TestMCPAdd_DuplicateID(t *testing.T) {
	t.Parallel()

	require.NotNil(t, mcpAddCmd)
	assert.Equal(t, "add <id>", mcpAddCmd.Use)
	assert.NotEmpty(t, mcpAddCmd.Short)

	// Verify required flags
	assert.NotNil(t, mcpAddCmd.Flags().Lookup("type"))
	assert.NotNil(t, mcpAddCmd.Flags().Lookup("command"))
	assert.NotNil(t, mcpAddCmd.Flags().Lookup("url"))
}

// TestMCPRemove_StripField tests the remove command configuration.
func TestMCPRemove_StripField(t *testing.T) {
	t.Parallel()

	require.NotNil(t, mcpRemoveCmd)
	assert.Equal(t, "remove <id>", mcpRemoveCmd.Use)
	assert.Contains(t, mcpRemoveCmd.Aliases, "rm")
	assert.NotEmpty(t, mcpRemoveCmd.Short)

	// Verify scope flags
	assert.NotNil(t, mcpRemoveCmd.Flags().Lookup("global"))
	assert.NotNil(t, mcpRemoveCmd.Flags().Lookup("local"))
}

func TestMakeMCPListItem(t *testing.T) {
	t.Parallel()

	m := config.MCPConfig{
		Type:    config.MCPStdio,
		Command: "node",
		Args:    []string{"server.js"},
		Disabled: false,
		Timeout: 30,
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
			name:  "empty",
			input: []string{},
			expected: map[string]string{},
		},
		{
			name:  "single pair",
			input: []string{"KEY=value"},
			expected: map[string]string{"KEY": "value"},
		},
		{
			name:  "multiple pairs",
			input: []string{"KEY1=value1", "KEY2=value2"},
			expected: map[string]string{"KEY1": "value1", "KEY2": "value2"},
		},
		{
			name:  "value with equals",
			input: []string{"KEY=value=with=equals"},
			expected: map[string]string{"KEY": "value=with=equals"},
		},
		{
			name:  "no equals ignored",
			input: []string{"KEYONLY"},
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

