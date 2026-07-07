package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	mcpmanager "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Inspect and manage Model Context Protocol servers",
	Long: `Manage the MCP server entries crush will connect to. MCP servers
provide additional tools that extend the agent's capabilities.

MCP server config lives under "mcp.<id>" in crush.json. Two scopes
exist and crush merges them at load time, workspace overriding global:

  --global   ~/.local/share/crush/crush.json   (or %LocalAppData%\crush on Windows)
  --local    ./.crush/crush.json               (next to the project)

If --global / --local is omitted the default is --global for write
operations and "both" for read operations.`,
}

var mcpListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured MCP servers across both scopes",
	Long: `Print the merged effective view of MCP servers (workspace overriding
global). Use --json for one JSON object per server. Use --grep to filter
by server ID, name, type, command, or URL (case-insensitive substring).

The TOOLS column shows the number of tools discovered for servers that
have been started in the current session, or "-" if the server has not
been reached.`,
	Example: `
crush mcp list
crush mcp list --json | jq 'select(.type=="stdio")'
crush mcp list --grep stdio
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		mcps := a.Config().MCP
		ids := make([]string, 0, len(mcps))
		for id := range mcps {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		grepPattern, _ := cmd.Flags().GetString("grep")

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, id := range ids {
				m := mcps[id]
				if grepPattern != "" {
					if !matchesMCPGrep(id, m, grepPattern) {
						continue
					}
				}
				item := makeMCPListItem(id, m)
				if err := enc.Encode(item); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATUS\tTOOLS\tCOMMAND/URL")
		for _, id := range ids {
			m := mcps[id]
			if grepPattern != "" {
				if !matchesMCPGrep(id, m, grepPattern) {
					continue
				}
			}
			status := "enabled"
			if m.Disabled {
				status = "disabled"
			}
			cmdOrURL := dash(m.Command)
			if m.URL != "" {
				cmdOrURL = dash(m.URL)
			}
			tools := "-"
			if info, ok := mcpmanager.GetState(id); ok {
				tools = fmt.Sprintf("%d", info.Counts.Tools)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				id,
				"-",
				dash(string(m.Type)),
				status,
				tools,
				cmdOrURL,
			)
		}
		return tw.Flush()
	},
}

var mcpShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Print an MCP server's full configuration",
	Long: `Display all configuration fields for a single MCP server: transport
type, command, args, env, headers, URL, disabled flag, and tool filters.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush mcp show my-server
crush mcp show my-server --json
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		m, ok := a.Config().MCP[id]
		if !ok {
			return fmt.Errorf("MCP server %q not configured", id)
		}

		item := makeMCPListItem(id, m)
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(item)
		}

		status := "enabled"
		if item.Disabled {
			status = "disabled"
		}
		fmt.Fprintf(os.Stdout, "id:       %s\n", item.ID)
		fmt.Fprintf(os.Stdout, "type:     %s\n", dash(item.Type))
		fmt.Fprintf(os.Stdout, "status:   %s\n", status)
		fmt.Fprintf(os.Stdout, "command:  %s\n", dash(item.Command))
		fmt.Fprintf(os.Stdout, "url:      %s\n", dash(item.URL))
		if len(item.Args) > 0 {
			fmt.Fprintf(os.Stdout, "args:     %v\n", item.Args)
		}
		if len(item.Env) > 0 {
			fmt.Fprintf(os.Stdout, "env:      %v\n", item.Env)
		}
		if len(item.Headers) > 0 {
			fmt.Fprintf(os.Stdout, "headers:  %v\n", item.Headers)
		}
		if len(item.DisabledTools) > 0 {
			fmt.Fprintf(os.Stdout, "disabled_tools: %v\n", item.DisabledTools)
		}
		if len(item.EnabledTools) > 0 {
			fmt.Fprintf(os.Stdout, "enabled_tools: %v\n", item.EnabledTools)
		}
		if item.Timeout > 0 {
			fmt.Fprintf(os.Stdout, "timeout:  %ds\n", item.Timeout)
		}
		return nil
	},
}

var mcpEnableCmd = &cobra.Command{
	Use:   "enable <id>",
	Short: "Enable an MCP server",
	Long: `Set mcp.<id>.disabled = false in the chosen scope. Warns if the
server is already enabled.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush mcp enable my-server
crush mcp enable my-server --global
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		m, ok := a.Config().MCP[id]
		if !ok {
			return fmt.Errorf("MCP server %q not found, see `crush mcp list`", id)
		}

		if !m.Disabled {
			fmt.Fprintf(os.Stderr, "MCP server %q is already enabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "mcp."+id+".disabled", false); err != nil {
			return fmt.Errorf("failed to enable MCP server: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ %s enabled\n", id)
		return nil
	},
}

var mcpDisableCmd = &cobra.Command{
	Use:   "disable <id>",
	Short: "Disable an MCP server",
	Long: `Set mcp.<id>.disabled = true in the chosen scope. Warns if the
server is already disabled.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush mcp disable my-server
crush mcp disable my-server --local
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		m, ok := a.Config().MCP[id]
		if !ok {
			return fmt.Errorf("MCP server %q not found, see `crush mcp list`", id)
		}

		if m.Disabled {
			fmt.Fprintf(os.Stderr, "MCP server %q is already disabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "mcp."+id+".disabled", true); err != nil {
			return fmt.Errorf("failed to disable MCP server: %w", err)
		}

		fmt.Fprintf(os.Stderr, "%s disabled\n", id)
		return nil
	},
}

var mcpRestartCmd = &cobra.Command{
	Use:   "restart <id>",
	Short: "Restart an MCP server",
	Long: `Request that an MCP server be restarted.

Hot-reload of running MCP servers is planned for a future release. For
now, use "crush web" or restart "crush run" for the change to take
effect.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush mcp restart my-server
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		if _, ok := a.Config().MCP[id]; !ok {
			return fmt.Errorf("MCP server %q not found, see `crush mcp list`", id)
		}

		fmt.Fprintf(os.Stderr, "restart requires crush web or crush run restart; hot-reload planned for future\n")
		return nil
	},
}

var mcpTestCmd = &cobra.Command{
	Use:   "test <id>",
	Short: "Test connectivity to an MCP server",
	Long: `Attempt to connect to an MCP server and verify it responds to a
tools/list request. For stdio servers this spawns the process; for
sse/http servers it sends an HTTP request.

Reports "ok: N tools" on success or "error: <diagnostic>" on failure.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush mcp test my-server
crush mcp test my-server --timeout 30s
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		m, ok := a.Config().MCP[id]
		if !ok {
			return fmt.Errorf("MCP server %q not found, see `crush mcp list`", id)
		}

		if m.Disabled {
			return fmt.Errorf("MCP server %q is disabled", id)
		}

		fmt.Fprintf(os.Stderr, "MCP connectivity test not yet implemented\n")
		fmt.Fprintf(os.Stderr, "Server type: %s\n", m.Type)
		return nil
	},
}

var mcpAddCmd = &cobra.Command{
	Use:   "add <id>",
	Short: "Add a new MCP server",
	Long: `Add a new MCP server to the chosen scope (default: global). Specify
the server type (stdio, sse, or http) and the appropriate connection
details.

For stdio servers, provide --command (and optionally --arg).
For sse/http servers, provide --url.
Set environment variables with --env and HTTP headers with --header.

Errors with "use crush mcp set to modify" if the ID already exists in
the chosen scope.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Add a stdio-based MCP server
crush mcp add my-server --type stdio --command "node" --arg server.js

# Add an HTTP-based MCP server
crush mcp add remote-server --type http --url http://localhost:3000/mcp

# Add with environment variables
crush mcp add my-server --type stdio --command "npx" --arg "mcp-server" --env "API_KEY=$MY_KEY"

# Add with authentication headers
crush mcp add auth-server --type http --url http://api.example.com/mcp --header "Authorization=Bearer $TOKEN"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]

		if _, exists := a.Config().MCP[id]; exists {
			return fmt.Errorf("MCP server %q already exists; use `crush mcp set %s` to modify", id, id)
		}

		typeStr, _ := cmd.Flags().GetString("type")
		if typeStr == "" {
			return fmt.Errorf("--type is required (stdio, sse, or http)")
		}

		mcpType := config.MCPType(typeStr)
		switch mcpType {
		case config.MCPStdio, config.MCPSSE, config.MCPHttp:
		default:
			return fmt.Errorf("invalid type %q; must be one of: stdio, sse, http", typeStr)
		}

		command, _ := cmd.Flags().GetString("command")
		mcpURL, _ := cmd.Flags().GetString("url")

		switch mcpType {
		case config.MCPStdio:
			if command == "" {
				return fmt.Errorf("--command is required for stdio type")
			}
		case config.MCPSSE, config.MCPHttp:
			if mcpURL == "" {
				return fmt.Errorf("--url is required for %s type", mcpType)
			}
		}

		fields := map[string]any{
			"mcp." + id + ".type": typeStr,
		}

		if command != "" {
			fields["mcp."+id+".command"] = command
		}
		if mcpURL != "" {
			fields["mcp."+id+".url"] = mcpURL
		}

		argSlice, _ := cmd.Flags().GetStringSlice("arg")
		if len(argSlice) > 0 {
			fields["mcp."+id+".args"] = argSlice
		}

		envStrs, _ := cmd.Flags().GetStringSlice("env")
		if len(envStrs) > 0 {
			envMap := parseKVPairs(envStrs)
			envJSON, _ := json.Marshal(envMap)
			fields["mcp."+id+".env"] = json.RawMessage(envJSON)
		}

		headers, _ := cmd.Flags().GetStringSlice("header")
		if len(headers) > 0 {
			headersMap := parseKVPairs(headers)
			headersJSON, _ := json.Marshal(headersMap)
			fields["mcp."+id+".headers"] = json.RawMessage(headersJSON)
		}

		if err := a.Store().SetConfigFields(scope, fields); err != nil {
			return fmt.Errorf("failed to add MCP server: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ MCP server %q created\n", id)
		return nil
	},
}

var mcpRemoveCmd = &cobra.Command{
	Use:     "remove <id>",
	Aliases: []string{"rm"},
	Short:   "Remove an MCP server from the chosen scope",
	Long: `Delete the mcp.<id> object from the targeted config file. The
server may still appear in "mcp list" if it is also defined in the
other scope (workspace fallback to global, or vice versa) — run
remove with the matching --global / --local to fully clear it.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush mcp remove my-server
crush mcp remove my-server --global
crush mcp rm old-server --local
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]

		if err := a.Store().RemoveConfigField(scope, "mcp."+id); err != nil {
			return fmt.Errorf("failed to remove MCP server from %s scope: %w", scope, err)
		}
		fmt.Fprintf(os.Stderr, "removed MCP server %q from %s scope\n", id, scope)
		return nil
	},
}

var mcpSetCmd = &cobra.Command{
	Use:   "set <id>",
	Short: "Update an MCP server's configuration",
	Long: `Set one or more MCP server fields in the chosen scope (default:
--global). Only the flags you pass are written — unset fields are left
untouched.

Use --disabled=true to disable without removing; --disabled=false to
re-enable.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Change the command for a stdio server
crush mcp set my-server --command "npx" --arg "mcp-server"

# Add headers to an HTTP server
crush mcp set remote-server --header "Authorization=Bearer $TOKEN"

# Disable a server without removing it
crush mcp set my-server --disabled=true

# Update the URL for an SSE server
crush mcp set events --url http://new-host:4000/sse
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		updates := map[string]any{}

		if cmd.Flags().Changed("command") {
			v, _ := cmd.Flags().GetString("command")
			updates["mcp."+id+".command"] = v
		}
		if cmd.Flags().Changed("url") {
			v, _ := cmd.Flags().GetString("url")
			updates["mcp."+id+".url"] = v
		}
		if cmd.Flags().Changed("type") {
			v, _ := cmd.Flags().GetString("type")
			updates["mcp."+id+".type"] = v
		}
		if cmd.Flags().Changed("disabled") {
			v, _ := cmd.Flags().GetBool("disabled")
			updates["mcp."+id+".disabled"] = v
		}

		argSlice, _ := cmd.Flags().GetStringSlice("arg")
		if len(argSlice) > 0 {
			updates["mcp."+id+".args"] = argSlice
		}

		envStrs, _ := cmd.Flags().GetStringSlice("env")
		if len(envStrs) > 0 {
			envMap := parseKVPairs(envStrs)
			envJSON, _ := json.Marshal(envMap)
			updates["mcp."+id+".env"] = json.RawMessage(envJSON)
		}

		headers, _ := cmd.Flags().GetStringSlice("header")
		if len(headers) > 0 {
			headersMap := parseKVPairs(headers)
			headersJSON, _ := json.Marshal(headersMap)
			updates["mcp."+id+".headers"] = json.RawMessage(headersJSON)
		}

		if len(updates) == 0 {
			return fmt.Errorf("no fields to set — pass at least one of --command/--url/--type/--arg/--env/--header/--disabled")
		}

		if err := a.Store().SetConfigFields(scope, updates); err != nil {
			return fmt.Errorf("failed to update MCP server config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %d field(s) to %s scope for MCP server %q\n", len(updates), scope, id)
		return nil
	},
}

func init() {
	mcpListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")
	mcpListCmd.Flags().String("grep", "", "Filter servers by id, type, command, or url (case-insensitive substring match)")
	mcpShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")
	mcpTestCmd.Flags().String("timeout", "10s", "Timeout for the connectivity test")

	for _, c := range []*cobra.Command{mcpEnableCmd, mcpDisableCmd, mcpAddCmd, mcpRemoveCmd, mcpSetCmd} {
		c.Flags().Bool("global", false, "Target the global config (~/.local/share/crush/crush.json). Default when neither --global nor --local is given.")
		c.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json).")
		c.MarkFlagsMutuallyExclusive("global", "local")
	}

	mcpAddCmd.Flags().String("type", "", "Server type: stdio, sse, or http (required)")
	mcpAddCmd.Flags().String("command", "", "Command to execute for stdio servers")
	mcpAddCmd.Flags().StringSlice("arg", nil, "Arguments to pass to the command (repeatable)")
	mcpAddCmd.Flags().StringSlice("env", nil, "Environment variables as KEY=VALUE (repeatable)")
	mcpAddCmd.Flags().StringSlice("header", nil, "HTTP headers as KEY=VALUE (repeatable)")
	mcpAddCmd.Flags().String("url", "", "URL for HTTP or SSE servers")

	mcpSetCmd.Flags().String("type", "", "Server type: stdio, sse, or http")
	mcpSetCmd.Flags().String("command", "", "Command to execute for stdio servers")
	mcpSetCmd.Flags().StringSlice("arg", nil, "Arguments to pass to the command (repeatable)")
	mcpSetCmd.Flags().StringSlice("env", nil, "Environment variables as KEY=VALUE (repeatable)")
	mcpSetCmd.Flags().StringSlice("header", nil, "HTTP headers as KEY=VALUE (repeatable)")
	mcpSetCmd.Flags().String("url", "", "URL for HTTP or SSE servers")
	mcpSetCmd.Flags().Bool("disabled", false, "Disable or enable the server")

	mcpCmd.AddCommand(mcpListCmd, mcpShowCmd, mcpEnableCmd, mcpDisableCmd, mcpRestartCmd, mcpTestCmd, mcpAddCmd, mcpRemoveCmd, mcpSetCmd)
	rootCmd.AddCommand(mcpCmd)
}

type mcpListItem struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	URL           string            `json:"url,omitempty"`
	Disabled      bool              `json:"disabled"`
	Env           map[string]string `json:"env,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	DisabledTools []string          `json:"disabled_tools,omitempty"`
	EnabledTools  []string          `json:"enabled_tools,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
	Source        string            `json:"source,omitempty"`
}

func makeMCPListItem(id string, m config.MCPConfig) mcpListItem {
	return mcpListItem{
		ID:            id,
		Type:          string(m.Type),
		Command:       m.Command,
		Args:          m.Args,
		URL:           m.URL,
		Disabled:      m.Disabled,
		Env:           m.Env,
		Headers:       m.Headers,
		DisabledTools: m.DisabledTools,
		EnabledTools:  m.EnabledTools,
		Timeout:       m.Timeout,
		Source:        string(m.Source),
	}
}

func matchesMCPGrep(id string, m config.MCPConfig, pattern string) bool {
	patternLower := strings.ToLower(pattern)
	fields := []string{
		strings.ToLower(id),
		strings.ToLower(string(m.Type)),
		strings.ToLower(m.Command),
		strings.ToLower(m.URL),
	}
	for _, field := range fields {
		if strings.Contains(field, patternLower) {
			return true
		}
	}
	return false
}

// parseKVPairs converts a slice of "KEY=VALUE" strings into a map.
func parseKVPairs(pairs []string) map[string]string {
	result := make(map[string]string)
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

// mcpTimeout returns the effective timeout for an MCP server config,
// defaulting to 15s when not explicitly set.
func mcpTimeout(m config.MCPConfig) time.Duration {
	if m.Timeout > 0 {
		return time.Duration(m.Timeout) * time.Second
	}
	return 15 * time.Second
}
