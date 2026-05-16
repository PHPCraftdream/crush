package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var modelsPresetCmd = &cobra.Command{
	Use:   "preset",
	Short: "Named (large, small) model pairs that can be swapped in atomically",
	Long: `A "preset" is a named pair of model selections. Save the current
large/small choices under a name with "preset save", switch back to
that pair later with "preset use", and inspect or remove pairs with
"preset list" / "preset delete".

Presets are kept in --global (default) or --local scope. Use them to
keep a handful of well-tuned combinations around — e.g. "cheap" for
day-to-day tasks, "frontier" for hard ones, "local" for an offline
ollama-only stack.`,
}

var modelsPresetSaveCmd = &cobra.Command{
	Use:   "save <name>",
	Short: "Snapshot the current large/small selection under <name>",
	Long: `Read the currently effective large and small models and write
them under model_presets.<name> in the chosen scope. Overwrites if a
preset with the same name already exists in that scope.

Pass --large=<model> / --small=<model> to save a custom pair instead
of the currently selected one (resolved with the same smart matcher
"crush models set" uses).`,
	Args: cobra.ExactArgs(1),
	Example: `
# Snapshot the current selection as "cheap"
crush models set --large gpt-4o-mini --small gpt-4o-mini
crush models preset save cheap

# Or save a custom pair directly
crush models preset save frontier --large openai/gpt-5@high --small openai/gpt-4o-mini
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

		largeStr, _ := cmd.Flags().GetString("large")
		smallStr, _ := cmd.Flags().GetString("small")

		preset := config.ModelPreset{}
		if largeStr != "" {
			modelPart, effort := splitModelEffort(largeStr)
			provider, modelID, rerr := a.ResolveModel(modelPart)
			if rerr != nil {
				return fmt.Errorf("--large: %w", rerr)
			}
			preset.Large = &config.SelectedModel{Provider: provider, Model: modelID, ReasoningEffort: effort}
		} else if cur, ok := a.Config().Models[config.SelectedModelTypeLarge]; ok {
			copy := cur
			preset.Large = &copy
		}
		if smallStr != "" {
			modelPart, effort := splitModelEffort(smallStr)
			provider, modelID, rerr := a.ResolveModel(modelPart)
			if rerr != nil {
				return fmt.Errorf("--small: %w", rerr)
			}
			preset.Small = &config.SelectedModel{Provider: provider, Model: modelID, ReasoningEffort: effort}
		} else if cur, ok := a.Config().Models[config.SelectedModelTypeSmall]; ok {
			copy := cur
			preset.Small = &copy
		}
		if preset.Large == nil && preset.Small == nil {
			return fmt.Errorf("no models to save (neither slot is currently selected and no --large/--small given)")
		}

		if err := a.Store().SetConfigField(scope, "model_presets."+args[0], preset); err != nil {
			return fmt.Errorf("failed to save preset: %w", err)
		}
		fmt.Fprintf(os.Stderr, "saved preset %q to %s scope\n", args[0], scope)
		return nil
	},
}

var modelsPresetUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Activate the (large, small) pair from preset <name>",
	Long: `Look up preset <name> (merged view, workspace-over-global) and
write both slots into the chosen scope (--global by default). Slots the
preset leaves empty are not touched.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush models preset use frontier
crush models preset use cheap --local
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

		preset, ok := a.Config().ModelPresets[args[0]]
		if !ok {
			return fmt.Errorf("preset %q not found (try \"crush models preset list\")", args[0])
		}

		wrote := 0
		if preset.Large != nil {
			if err := a.Store().UpdatePreferredModel(scope, config.SelectedModelTypeLarge, *preset.Large); err != nil {
				return fmt.Errorf("failed to apply large slot: %w", err)
			}
			fmt.Fprintf(os.Stderr, "set large = %s/%s in %s scope\n", preset.Large.Provider, preset.Large.Model, scope)
			wrote++
		}
		if preset.Small != nil {
			if err := a.Store().UpdatePreferredModel(scope, config.SelectedModelTypeSmall, *preset.Small); err != nil {
				return fmt.Errorf("failed to apply small slot: %w", err)
			}
			fmt.Fprintf(os.Stderr, "set small = %s/%s in %s scope\n", preset.Small.Provider, preset.Small.Model, scope)
			wrote++
		}
		if wrote == 0 {
			return fmt.Errorf("preset %q is empty (both slots nil)", args[0])
		}
		return nil
	},
}

var modelsPresetListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show all defined model presets (merged view, --json optional)",
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		names := make([]string, 0, len(a.Config().ModelPresets))
		for n := range a.Config().ModelPresets {
			names = append(names, n)
		}
		sort.Strings(names)

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, n := range names {
				p := a.Config().ModelPresets[n]
				if err := enc.Encode(map[string]any{"name": n, "preset": p}); err != nil {
					return err
				}
			}
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tLARGE\tSMALL")
		for _, n := range names {
			p := a.Config().ModelPresets[n]
			fmt.Fprintf(tw, "%s\t%s\t%s\n", n, formatSel(p.Large), formatSel(p.Small))
		}
		return tw.Flush()
	},
}

var modelsPresetDeleteCmd = &cobra.Command{
	Use:     "delete <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a model preset from the chosen scope",
	Args:    cobra.ExactArgs(1),
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
		if err := a.Store().RemoveConfigField(scope, "model_presets."+args[0]); err != nil {
			return fmt.Errorf("failed to delete preset from %s scope: %w", scope, err)
		}
		fmt.Fprintf(os.Stderr, "deleted preset %q from %s scope\n", args[0], scope)
		return nil
	},
}

func init() {
	modelsPresetListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")
	for _, c := range []*cobra.Command{modelsPresetSaveCmd, modelsPresetUseCmd, modelsPresetDeleteCmd} {
		c.Flags().Bool("global", false, "Target the global config (default)")
		c.Flags().Bool("local", false, "Target the workspace config")
		c.MarkFlagsMutuallyExclusive("global", "local")
	}
	modelsPresetSaveCmd.Flags().String("large", "", "Override the large slot in the saved preset (defaults to currently selected)")
	modelsPresetSaveCmd.Flags().String("small", "", "Override the small slot (defaults to currently selected)")

	modelsPresetCmd.AddCommand(modelsPresetSaveCmd, modelsPresetUseCmd, modelsPresetListCmd, modelsPresetDeleteCmd)
	modelsCmd.AddCommand(modelsPresetCmd)
}

func formatSel(s *config.SelectedModel) string {
	if s == nil {
		return "-"
	}
	out := s.Provider + "/" + s.Model
	if s.ReasoningEffort != "" {
		out += "@" + s.ReasoningEffort
	}
	return out
}
