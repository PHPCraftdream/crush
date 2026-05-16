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
			out[string(t)] = map[string]any{
				"provider":         m.Provider,
				"model":            m.Model,
				"reasoning_effort": m.ReasoningEffort,
			}
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
			fmt.Fprintf(os.Stdout, "%-6s: %s / %s%s\n", t, v["provider"], v["model"], eff)
		}
		return nil
	},
}

var modelsSetCmd = &cobra.Command{
	Use:   "set <large|small> <model>",
	Short: "Pin the large or small model in --global (default) or --local scope",
	Long: `Pin one of the selected models. <model> is resolved with the same
smart matcher "crush run --model" uses:

  - bare name ("gpt-4o"): searched across every configured provider.
    If it lives in more than one, you get an ambiguity error listing
    them — re-run with "provider/model" to disambiguate.
  - "provider/model": exact provider, exact model id.

The chosen scope is written to crush.json:
  --global (default)  ~/.local/share/crush/crush.json
  --local             ./.crush/crush.json
`,
	Args: cobra.ExactArgs(2),
	Example: `
# Pin gpt-5 globally
crush models set large gpt-5

# Disambiguate when two providers carry the same model id
crush models set large openai/gpt-5

# Override just for this workspace
crush models set small --local groq/llama-3.3-70b

# Optional reasoning effort for OpenAI/Anthropic reasoning-capable models
crush models set large openai/gpt-5 --reasoning-effort high
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		role := args[0]
		modelStr := args[1]

		var typ config.SelectedModelType
		switch role {
		case "large":
			typ = config.SelectedModelTypeLarge
		case "small":
			typ = config.SelectedModelTypeSmall
		default:
			return fmt.Errorf("first arg must be \"large\" or \"small\", got %q", role)
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

		provider, modelID, err := a.ResolveModel(modelStr)
		if err != nil {
			return err
		}

		sel := config.SelectedModel{Provider: provider, Model: modelID}
		if r, _ := cmd.Flags().GetString("reasoning-effort"); r != "" {
			sel.ReasoningEffort = r
		}

		if err := a.Store().UpdatePreferredModel(scope, typ, sel); err != nil {
			return fmt.Errorf("failed to write selected model: %w", err)
		}
		fmt.Fprintf(os.Stderr, "set %s model to %s/%s in %s scope\n", role, provider, modelID, scope)
		return nil
	},
}

func init() {
	modelsShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")

	modelsSetCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsSetCmd.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json)")
	modelsSetCmd.Flags().String("reasoning-effort", "", "Optional: low|medium|high — only meaningful for reasoning-capable models")
	modelsSetCmd.MarkFlagsMutuallyExclusive("global", "local")

	modelsCmd.AddCommand(modelsShowCmd, modelsSetCmd)
}
