package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var providersCmd = &cobra.Command{
	Use:   "providers",
	Short: "Inspect and edit LLM-provider configuration",
	Long: `Manage the provider entries crush will use for chat completions.

Provider config lives under "providers.<id>" in crush.json. Two scopes
exist and crush merges them at load time, workspace overriding global:

  --global   ~/.local/share/crush/crush.json   (or %LocalAppData%\crush on Windows)
  --local    ./.crush/crush.json               (next to the project)

If --global / --local is omitted the default is --global for write
operations and "both" for read operations.`,
}

var providersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured providers across both scopes",
	Long: `Print the merged effective view of providers (workspace overriding
global). Use --json for one NDJSON object per provider. API keys are
always masked — only the last 4 chars are shown.`,
	Example: `
crush providers list
crush providers list --json | jq 'select(.api_key_present)'
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		providers := a.Config().Providers.Copy()
		// Stable order.
		ids := make([]string, 0, len(providers))
		for id := range providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, id := range ids {
				p := providers[id]
				if err := enc.Encode(makeProviderListItem(id, p)); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tTYPE\tDISABLED\tAPI KEY\tBASE URL\tMODELS")
		for _, id := range ids {
			p := providers[id]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%s\t%d\n",
				id,
				dash(p.Name),
				dash(string(p.Type)),
				p.Disable,
				maskKey(p.APIKey),
				dash(p.BaseURL),
				len(p.Models),
			)
		}
		return tw.Flush()
	},
}

var providersShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Print a provider's effective config (api_key masked)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		p, ok := a.Config().Providers.Get(args[0])
		if !ok {
			return fmt.Errorf("provider %q not configured", args[0])
		}
		item := makeProviderListItem(args[0], p)
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(item)
		}
		fmt.Fprintf(os.Stdout, "id:          %s\n", item.ID)
		fmt.Fprintf(os.Stdout, "name:        %s\n", dash(item.Name))
		fmt.Fprintf(os.Stdout, "type:        %s\n", dash(item.Type))
		fmt.Fprintf(os.Stdout, "disabled:    %t\n", item.Disabled)
		fmt.Fprintf(os.Stdout, "api_key:     %s (present: %t)\n", item.APIKey, item.APIKeyPresent)
		fmt.Fprintf(os.Stdout, "base_url:    %s\n", dash(item.BaseURL))
		fmt.Fprintf(os.Stdout, "models:      %d\n", item.Models)
		fmt.Fprintf(os.Stdout, "oauth:       %t\n", item.HasOAuth)
		return nil
	},
}

var providersSetCmd = &cobra.Command{
	Use:   "set <id>",
	Short: "Write provider fields to the chosen scope",
	Long: `Set one or more provider fields in --global (default) or --local
scope. Only the flags you pass are written — unset fields are left
untouched, so you can update just an API key without erasing the
base URL.

Pass --disabled=true to disable a provider without losing its
credentials; --disabled=false to re-enable.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Set api key in global config (default scope)
crush providers set openai --api-key=$OPENAI_API_KEY

# Pin a custom base URL just for this workspace
crush providers set openai --local --base-url=http://localhost:11434/v1

# Disable a provider without removing it
crush providers set hyper --disabled=true
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		id := args[0]
		updates := map[string]any{}
		if cmd.Flags().Changed("api-key") {
			v, _ := cmd.Flags().GetString("api-key")
			updates["providers."+id+".api_key"] = v
		}
		if cmd.Flags().Changed("base-url") {
			v, _ := cmd.Flags().GetString("base-url")
			updates["providers."+id+".base_url"] = v
		}
		if cmd.Flags().Changed("type") {
			v, _ := cmd.Flags().GetString("type")
			updates["providers."+id+".type"] = v
		}
		if cmd.Flags().Changed("name") {
			v, _ := cmd.Flags().GetString("name")
			updates["providers."+id+".name"] = v
		}
		if cmd.Flags().Changed("disabled") {
			v, _ := cmd.Flags().GetBool("disabled")
			updates["providers."+id+".disable"] = v
		}
		if len(updates) == 0 {
			return fmt.Errorf("no fields to set — pass at least one of --api-key/--base-url/--type/--name/--disabled")
		}

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		if err := a.Store().SetConfigFields(scope, updates); err != nil {
			return fmt.Errorf("failed to write provider config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %d field(s) to %s scope for provider %q\n", len(updates), scope, id)
		return nil
	},
}

var providersUnsetCmd = &cobra.Command{
	Use:     "unset <id>",
	Aliases: []string{"remove", "rm"},
	Short:   "Remove a provider entry from the chosen scope",
	Long: `Delete the providers.<id> object from the targeted config file. The
provider may still appear in "providers list" if it is also defined in
the other scope (workspace fallback to global, or vice versa) — run
unset with the matching --global / --local to fully clear it.`,
	Args: cobra.ExactArgs(1),
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

		if err := a.Store().RemoveConfigField(scope, "providers."+args[0]); err != nil {
			return fmt.Errorf("failed to remove provider from %s scope: %w", scope, err)
		}
		fmt.Fprintf(os.Stderr, "removed provider %q from %s scope\n", args[0], scope)
		return nil
	},
}

func init() {
	providersListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")
	providersShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")

	for _, c := range []*cobra.Command{providersSetCmd, providersUnsetCmd} {
		c.Flags().Bool("global", false, "Target the global config (~/.local/share/crush/crush.json). Default when neither --global nor --local is given.")
		c.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json).")
		c.MarkFlagsMutuallyExclusive("global", "local")
	}
	providersSetCmd.Flags().String("api-key", "", "API key for the provider (literal or $VAR — the latter expands at config-load time)")
	providersSetCmd.Flags().String("base-url", "", "Base URL for the provider's API")
	providersSetCmd.Flags().String("type", "", "Provider type: openai|openai-compat|anthropic|gemini|azure|vertexai")
	providersSetCmd.Flags().String("name", "", "Human-readable display name shown in the WUI")
	providersSetCmd.Flags().Bool("disabled", false, "Mark provider as disabled (kept in config, ignored at runtime)")

	providersCmd.AddCommand(providersListCmd, providersShowCmd, providersSetCmd, providersUnsetCmd)
	rootCmd.AddCommand(providersCmd)
}

// providerListItem is the JSON shape of providers list / show. Kept
// separate from config.ProviderConfig so api_key is never serialised as
// plaintext by accident.
type providerListItem struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Type          string `json:"type,omitempty"`
	APIKey        string `json:"api_key"` // masked
	APIKeyPresent bool   `json:"api_key_present"`
	BaseURL       string `json:"base_url,omitempty"`
	Disabled      bool   `json:"disabled"`
	Models        int    `json:"models"`
	HasOAuth      bool   `json:"oauth"`
}

func makeProviderListItem(id string, p config.ProviderConfig) providerListItem {
	return providerListItem{
		ID:            id,
		Name:          p.Name,
		Type:          string(p.Type),
		APIKey:        maskKey(p.APIKey),
		APIKeyPresent: p.APIKey != "",
		BaseURL:       p.BaseURL,
		Disabled:      p.Disable,
		Models:        len(p.Models),
		HasOAuth:      p.OAuthToken != nil,
	}
}

func scopeFromFlags(cmd *cobra.Command, def config.Scope) (config.Scope, error) {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	switch {
	case global && local:
		return def, fmt.Errorf("--global and --local are mutually exclusive")
	case global:
		return config.ScopeGlobal, nil
	case local:
		return config.ScopeWorkspace, nil
	default:
		return def, nil
	}
}

func maskKey(k string) string {
	if k == "" {
		return "-"
	}
	if strings.HasPrefix(k, "$") {
		// Env-template still unresolved — show the template literally so
		// it's obvious where the key comes from.
		return k
	}
	if len(k) <= 4 {
		return "****"
	}
	return strings.Repeat("*", 4) + k[len(k)-4:]
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
