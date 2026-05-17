package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var modelsShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the currently selected large and small models",
	Long: `Print the currently effective large/small model assignments. The
"selected" view is the merge of workspace config over global config;
each row carries the scope it came from so you can tell where to write
your --local override.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		out := map[string]any{}
		for _, t := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
			m, ok := a.Config().Models[t]
			if !ok {
				out[string(t)] = nil
				continue
			}
			row := map[string]any{
				"provider":         m.Provider,
				"model":            m.Model,
				"reasoning_effort": m.ReasoningEffort,
			}
			// Fork patch (orchestrator UX): pricing + context window
			// pulled from the catwalk catalog so the operator sees the
			// $/1M-token trade-off without first running a turn and
			// inspecting the envelope's usage. Missing catalog entry
			// (custom/local model) leaves the price fields off.
			if cat := a.Config().GetModel(m.Provider, m.Model); cat != nil {
				row["context_window"] = cat.ContextWindow
				row["cost_per_1m_in"] = cat.CostPer1MIn
				row["cost_per_1m_out"] = cat.CostPer1MOut
				if cat.CostPer1MInCached != 0 {
					row["cost_per_1m_in_cached"] = cat.CostPer1MInCached
				}
				if cat.CostPer1MOutCached != 0 {
					row["cost_per_1m_out_cached"] = cat.CostPer1MOutCached
				}
			}
			out[string(t)] = row
		}

		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		for _, t := range []string{"large", "small"} {
			v, ok := out[t].(map[string]any)
			if !ok || v == nil {
				fmt.Fprintf(os.Stdout, "%-6s: (not set)\n", t)
				continue
			}
			eff := ""
			if r, _ := v["reasoning_effort"].(string); r != "" {
				eff = " effort=" + r
			}
			// Fork patch (orchestrator UX): print pricing on a second
			// indented line when the catalog entry exists. Skipped
			// silently for custom/local models so the human-readable
			// view never blows up.
			priceSuffix := ""
			if ctx, ok := v["context_window"].(int64); ok && ctx > 0 {
				priceSuffix += fmt.Sprintf(" ctx=%s", humanK(ctx))
			}
			fmt.Fprintf(os.Stdout, "%-6s: %s / %s%s%s\n", t, v["provider"], v["model"], eff, priceSuffix)
			in, hasIn := v["cost_per_1m_in"].(float64)
			outP, hasOut := v["cost_per_1m_out"].(float64)
			if hasIn && hasOut && (in != 0 || outP != 0) {
				fmt.Fprintf(os.Stdout, "        $%.2f / 1M in, $%.2f / 1M out", in, outP)
				if c, ok := v["cost_per_1m_in_cached"].(float64); ok && c != 0 {
					fmt.Fprintf(os.Stdout, " (cached-in $%.2f", c)
					if co, ok := v["cost_per_1m_out_cached"].(float64); ok && co != 0 {
						fmt.Fprintf(os.Stdout, ", cached-out $%.2f", co)
					}
					fmt.Fprint(os.Stdout, ")")
				}
				fmt.Fprintln(os.Stdout)
			}
		}
		return nil
	},
}

// humanK formats a context window as a compact "128k" / "1M". Avoids
// pulling in a units helper for a one-call site.
func humanK(n int64) string {
	switch {
	case n >= 1_000_000:
		if n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		if n%1_000 == 0 {
			return fmt.Sprintf("%dk", n/1_000)
		}
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

var modelsSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Pin the large and/or small models in --global (default) or --local scope",
	Long: `Pin the large and/or small slots in one shot. Pass --large and/or
--small; each is resolved with the same smart matcher --model on
"crush run" uses:

  - bare name ("gpt-4o"): searched across every configured provider.
    If it lives in more than one you get an ambiguity error listing
    them — re-run with "provider/model" to disambiguate.
  - "provider/model": exact provider, exact model id.

Reasoning-capable models accept an optional effort with
"<model>@<low|medium|high>" — for example "openai/gpt-5@high".

The chosen scope is written to crush.json:
  --global (default)  ~/.local/share/crush/crush.json
  --local             ./.crush/crush.json
`,
	Args: cobra.NoArgs,
	Example: `
# Pin both at once globally
crush models set --large gpt-5 --small gpt-4o-mini

# Disambiguate when a model id lives in multiple providers
crush models set --large openai/gpt-5

# Workspace-only override
crush models set --local --small groq/llama-3.3-70b

# Reasoning effort via @ suffix
crush models set --large openai/gpt-5@high
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		largeStr, _ := cmd.Flags().GetString("large")
		smallStr, _ := cmd.Flags().GetString("small")
		if largeStr == "" && smallStr == "" {
			return fmt.Errorf("at least one of --large / --small must be set")
		}

		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		updates := []struct {
			typ   config.SelectedModelType
			raw   string
			label string
		}{
			{config.SelectedModelTypeLarge, largeStr, "large"},
			{config.SelectedModelTypeSmall, smallStr, "small"},
		}
		for _, u := range updates {
			if u.raw == "" {
				continue
			}
			modelPart, effort := splitModelEffort(u.raw)
			provider, modelID, rerr := a.ResolveModel(modelPart)
			if rerr != nil {
				return fmt.Errorf("--%s: %w", u.label, rerr)
			}
			sel := config.SelectedModel{Provider: provider, Model: modelID, ReasoningEffort: effort}
			if werr := a.Store().UpdatePreferredModel(scope, u.typ, sel); werr != nil {
				return fmt.Errorf("--%s: failed to write: %w", u.label, werr)
			}
			fmt.Fprintf(os.Stderr, "set %s = %s/%s", u.label, provider, modelID)
			if effort != "" {
				fmt.Fprintf(os.Stderr, " (effort %s)", effort)
			}
			fmt.Fprintf(os.Stderr, " in %s scope\n", scope)
		}
		return nil
	},
}

// splitModelEffort splits "openai/gpt-5@high" into ("openai/gpt-5", "high").
// If no "@", returns (s, ""). The @ form is a CLI-only convenience so the
// user can pin reasoning effort in the same flag value.
func splitModelEffort(s string) (model, effort string) {
	at := -1
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return s, ""
	}
	return s[:at], s[at+1:]
}

func init() {
	modelsShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")

	modelsSetCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsSetCmd.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json)")
	modelsSetCmd.Flags().String("large", "", "Model for the \"smart/slow\" slot. Accepts \"model\", \"provider/model\", or either with \"@low|@medium|@high\" effort suffix.")
	modelsSetCmd.Flags().String("small", "", "Model for the \"fast/cheap\" slot. Same syntax as --large.")
	modelsSetCmd.MarkFlagsMutuallyExclusive("global", "local")

	modelsCmd.AddCommand(modelsShowCmd, modelsSetCmd)
}
