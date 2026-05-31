package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/app"
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

		grepPattern, _ := cmd.Flags().GetString("grep")
		grepLower := strings.ToLower(grepPattern)

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, id := range ids {
				p := providers[id]
				if grepPattern != "" {
					if !matchesGrep(id, p, grepLower) {
						continue
					}
				}
				if err := enc.Encode(makeProviderListItem(id, p)); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATUS\tMODELS\tAPI_KEY\tBASE_URL")
		for _, id := range ids {
			p := providers[id]
			if grepPattern != "" {
				if !matchesGrep(id, p, grepLower) {
					continue
				}
			}
			status := "enabled"
			if p.Disable {
				status = "disabled"
			}
			modelCount := "—"
			if len(p.Models) > 0 {
				modelCount = fmt.Sprintf("%d", len(p.Models))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				id,
				dash(p.Name),
				dash(string(p.Type)),
				status,
				modelCount,
				maskKey(p.APIKey),
				dash(p.BaseURL),
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
		status := "enabled"
		if item.Disabled {
			status = "disabled"
		}
		fmt.Fprintf(os.Stdout, "id:          %s\n", item.ID)
		fmt.Fprintf(os.Stdout, "name:        %s\n", dash(item.Name))
		fmt.Fprintf(os.Stdout, "type:        %s\n", dash(item.Type))
		fmt.Fprintf(os.Stdout, "status:      %s\n", status)
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

var providersAddCmd = &cobra.Command{
	Use:   "add <id>",
	Short: "Add a new provider",
	Long: `Add a new provider to the chosen scope (default: global). Specify the
provider type, name, and optionally a base URL and API key.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Add a catwalk-known provider (Z.AI)
crush providers add zai --name "Z.AI" --type openai-compat --base-url https://api.z.ai --api-key $ZAI_API_KEY

# Add a custom OpenAI-compatible provider
crush providers add local-llm --name "Local LLM" --type openai-compat --base-url http://localhost:8000/v1 --api-key none`,
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
		name, _ := cmd.Flags().GetString("name")
		typeStr, _ := cmd.Flags().GetString("type")
		baseURL, _ := cmd.Flags().GetString("base-url")
		apiKey, _ := cmd.Flags().GetString("api-key")
		enable, _ := cmd.Flags().GetBool("enable")

		if name == "" {
			return fmt.Errorf("--name is required")
		}
		if typeStr == "" {
			return fmt.Errorf("--type is required")
		}

		// Check if provider already exists
		if _, exists := a.Config().Providers.Get(id); exists {
			return fmt.Errorf("provider %q already exists", id)
		}

		provType := catwalk.Type(typeStr)

		if provType == "cli" {
			return fmt.Errorf("adding new CLI providers requires editing internal/agent/cliprovider/provider.go directly")
		}

		// Validate provider type
		validTypes := catwalk.KnownProviderTypes()
		validTypes = append(validTypes, "openai-compat")
		isValid := false
		for _, t := range validTypes {
			if t == provType {
				isValid = true
				break
			}
		}
		if !isValid {
			fmt.Fprintf(os.Stderr, "Supported provider types: %v\n", validTypes)
			return fmt.Errorf("unsupported provider type %q", typeStr)
		}

		// If base-url not provided for catwalk-known, try to get from catwalk
		if baseURL == "" {
			for _, known := range a.Store().KnownProviders() {
				if known.ID == catwalk.InferenceProvider(id) {
					baseURL = known.APIEndpoint
					break
				}
			}
		}

		pc := config.ProviderConfig{
			ID:      id,
			Name:    name,
			Type:    provType,
			BaseURL: baseURL,
			APIKey:  apiKey,
			Disable: !enable,
		}

		fields := map[string]any{
			"providers." + id + ".name":    name,
			"providers." + id + ".type":    typeStr,
			"providers." + id + ".disable": pc.Disable,
		}
		if baseURL != "" {
			fields["providers."+id+".base_url"] = baseURL
		}
		if apiKey != "" {
			fields["providers."+id+".api_key"] = apiKey
		}

		if err := a.Store().SetConfigFields(scope, fields); err != nil {
			return fmt.Errorf("failed to add provider: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ %s created\n", id)

		// Test connection if API key provided
		if apiKey != "" && !strings.HasPrefix(apiKey, "$") {
			if err := pc.TestConnection(a.Store().Resolver()); err != nil {
				fmt.Fprintf(os.Stderr, "✗ connection failed: %v (but provider saved; fix and re-enable)\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✓ connection verified\n")
			}
		}

		// Fetch models if enabled
		if enable {
			if err := updateSingleProvider(a, id); err != nil {
				fmt.Fprintf(os.Stderr, "note: failed to fetch models: %v\n", err)
			}
		}

		return nil
	},
}

var providersUpdateCmd = &cobra.Command{
	Use:   "update [<id> | --all]",
	Short: "Refresh provider models from the API",
	Long: `Fetch the latest model list from a provider's API and update the local
configuration. Shows a summary of added/removed models.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		all, _ := cmd.Flags().GetBool("all")

		if all && len(args) > 0 {
			return fmt.Errorf("--all and <id> are mutually exclusive")
		}

		if !all && len(args) == 0 {
			return fmt.Errorf("either specify <id> or use --all")
		}

		if all {
			return updateAllProviders(a)
		}

		return updateSingleProvider(a, args[0])
	},
}

func updateSingleProvider(a *app.App, id string) error {
	p, ok := a.Config().Providers.Get(id)
	if !ok {
		return fmt.Errorf("provider %q not found", id)
	}

	oldModels := p.Models
	newModels, warnings, err := fetchModels(a, p)
	if err != nil {
		return err
	}

	added, removed := computeDiff(oldModels, newModels)

	oldCount := len(oldModels)
	newCount := len(newModels)

	var diffStr strings.Builder
	if newCount > oldCount {
		diffStr.WriteString(fmt.Sprintf(" (+%d", newCount-oldCount))
		if len(added) > 0 && len(added) <= 3 {
			diffStr.WriteString(": ")
			for i, m := range added {
				if i > 0 {
					diffStr.WriteString(", ")
				}
				diffStr.WriteString(m.ID)
			}
		}
		diffStr.WriteString(")")
	} else if newCount < oldCount {
		diffStr.WriteString(fmt.Sprintf(" (-%d", oldCount-newCount))
		if len(removed) > 0 && len(removed) <= 3 {
			diffStr.WriteString(": ")
			for i, m := range removed {
				if i > 0 {
					diffStr.WriteString(", ")
				}
				diffStr.WriteString(m.ID)
			}
		}
		diffStr.WriteString(")")
	}

	fmt.Fprintf(os.Stderr, "%s: %d → %d models%s\n", id, oldCount, newCount, diffStr.String())

	// Check for orphaned preferred slots
	cfg := a.Config()
	for modelType, model := range cfg.Models {
		if model.Provider != id {
			continue
		}
		for _, rm := range removed {
			if rm.ID == model.Model {
				slotName := "smart"
				if modelType == config.SelectedModelTypeSmall {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "WARN: preferred %s = %s/%s no longer exists after update — your '%s' slot is broken. Run `crush models use <large> <small>` to fix.\n", slotName, id, model.Model, slotName)
			}
		}
	}

	// Save updated models to config
	modelsJSON, _ := json.Marshal(newModels)
	updates := map[string]any{
		"providers." + id + ".models": json.RawMessage(modelsJSON),
	}
	if err := a.Store().SetConfigFields(config.ScopeGlobal, updates); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save updated models: %v\n", err)
	}

	// Print warnings
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "note: %s\n", w)
	}

	return nil
}

func updateAllProviders(a *app.App) error {
	providers := a.Config().EnabledProviders()
	count := 0
	for _, p := range providers {
		if err := updateSingleProvider(a, p.ID); err != nil {
			fmt.Fprintf(os.Stderr, "error updating %s: %v\n", p.ID, err)
			continue
		}
		count++
	}
	fmt.Fprintf(os.Stderr, "Updated %d provider(s)\n", count)
	return nil
}

var providersEnableCmd = &cobra.Command{
	Use:   "enable <id>",
	Short: "Enable a provider and refresh its model list",
	Args:  cobra.ExactArgs(1),
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
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found, see `crush providers list`", id)
		}

		if !p.Disable {
			fmt.Fprintf(os.Stderr, "provider %q is already enabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "providers."+id+".disable", false); err != nil {
			return fmt.Errorf("failed to enable provider: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ %s enabled\n", id)
		return nil
	},
}

var providersDisableCmd = &cobra.Command{
	Use:   "disable <id>",
	Short: "Disable a provider",
	Args:  cobra.ExactArgs(1),
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
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found, see `crush providers list`", id)
		}

		if p.Disable {
			fmt.Fprintf(os.Stderr, "provider %q is already disabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "providers."+id+".disable", true); err != nil {
			return fmt.Errorf("failed to disable provider: %w", err)
		}

		// Check if preferred slots use this provider
		cfg := a.Config()
		for modelType, model := range cfg.Models {
			if model.Provider == id {
				slotName := "smart"
				if modelType == config.SelectedModelTypeSmall {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "warning: %s slot was using %s/%s; that slot is now broken. Run `crush models use <large> <small>` to fix.\n", slotName, id, model.Model)
			}
		}

		fmt.Fprintf(os.Stderr, "%s disabled\n", id)
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

		id := args[0]

		// Check if preferred slots use this provider and warn
		cfg := a.Config()
		for modelType, model := range cfg.Models {
			if model.Provider == id {
				slotName := "smart"
				if modelType == config.SelectedModelTypeSmall {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "warning: %s slot was using %s/%s; that slot is now broken. Run `crush models use <large> <small>` to fix.\n", slotName, id, model.Model)
			}
		}

		if err := a.Store().RemoveConfigField(scope, "providers."+id); err != nil {
			return fmt.Errorf("failed to remove provider from %s scope: %w", scope, err)
		}
		fmt.Fprintf(os.Stderr, "removed provider %q from %s scope\n", id, scope)
		return nil
	},
}

var providersGrepCmd = &cobra.Command{
	Use:   "grep <pattern>",
	Short: "Filter providers by id, name, or type (sugar for `list --grep <pattern>`)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		needle := strings.ToLower(args[0])
		providers := a.Config().Providers.Copy()
		ids := make([]string, 0, len(providers))
		for id := range providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID	NAME	TYPE	STATUS	MODELS	API_KEY	BASE_URL")
		matched := 0
		for _, id := range ids {
			p := providers[id]
			if !matchesGrep(id, p, needle) {
				continue
			}
			status := "enabled"
			if p.Disable {
				status = "disabled"
			}
			modelCount := "—"
			if len(p.Models) > 0 {
				modelCount = fmt.Sprintf("%d", len(p.Models))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, dash(p.Name), dash(string(p.Type)), status, modelCount, maskKey(p.APIKey), dash(p.BaseURL))
			matched++
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if matched == 0 {
			fmt.Fprintf(os.Stderr, "no providers matched %q\n", args[0])
		}
		return nil
	},
}

func init() {
	providersListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")
	providersListCmd.Flags().String("grep", "", "Filter providers by id, name, or type (case-insensitive substring match)")
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

	for _, c := range []*cobra.Command{providersEnableCmd, providersDisableCmd, providersAddCmd} {
		c.Flags().Bool("global", false, "Target the global config (~/.local/share/crush/crush.json). Default when neither --global nor --local is given.")
		c.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json).")
		c.MarkFlagsMutuallyExclusive("global", "local")
	}

	providersAddCmd.Flags().String("name", "", "Human-readable name for the provider (required)")
	providersAddCmd.Flags().String("type", "", "Provider type: openai|openai-compat|anthropic|gemini|azure|vertexai|bedrock|xai|zai|groq|openrouter|synthetic|huggingface|copilot|vercel (required)")
	providersAddCmd.Flags().String("base-url", "", "Base URL for the provider's API (optional for catwalk-known providers)")
	providersAddCmd.Flags().String("api-key", "", "API key for the provider (optional, can be set later)")
	providersAddCmd.Flags().Bool("enable", true, "Enable the provider after creation (default: true)")

	providersUpdateCmd.Flags().Bool("all", false, "Update all enabled providers")

	providersCmd.AddCommand(providersListCmd, providersShowCmd, providersSetCmd, providersAddCmd, providersEnableCmd, providersDisableCmd, providersUnsetCmd, providersUpdateCmd, providersGrepCmd)
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

func matchesGrep(id string, p config.ProviderConfig, patternLower string) bool {
	fields := []string{
		strings.ToLower(id),
		strings.ToLower(p.Name),
		strings.ToLower(string(p.Type)),
	}
	for _, field := range fields {
		if strings.Contains(field, patternLower) {
			return true
		}
	}
	return false
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
