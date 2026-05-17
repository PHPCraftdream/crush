// Fork patch: batch 11 — `crush models use <large> <small>` replaces the older
// `crush models set --large X --small Y` with positional args + atom registry.
package cmd

import (
	"fmt"
	"os"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var modelsUseCmd = &cobra.Command{
	Use:   "use <large> <small>",
	Short: "Set the large and small slots in one shot from atom names",
	Long: `Activate a (large, small) pair using the atom syntax. Each argument is
either an atom name (e.g. "opus-high", "glm5_turbo") OR a raw
"provider/model[@level]" string for models not in the atom registry.

The chosen scope is written to crush.json:
  --global (default)  ~/.local/share/crush/crush.json
  --local             ./.crush/crush.json

The current value in the OTHER scope is preserved; effective resolution
remains "local if set, else global".

See ` + "`crush models list`" + ` for the full atom table.`,
	Args: cobra.ExactArgs(2),
	Example: `
crush models use opus-high sonnet-low                # both Anthropic
crush models use glm5_1 glm5_turbo                   # both Z.AI
crush models use opus-max glm5_turbo                 # mixed providers
crush models use --local haiku-xhigh haiku-low       # workspace-only
crush models use openai/gpt-5@high zai/glm-5-turbo   # raw provider/model fallback
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

		resolve := func(modelPart string) (string, string, error) {
			provider, modelID, rerr := a.ResolveModel(modelPart)
			return provider, modelID, rerr
		}

		largeSel, lerr := parseAtomOrRaw(args[0], resolve)
		if lerr != nil {
			return fmt.Errorf("large: %w", lerr)
		}
		smallSel, serr := parseAtomOrRaw(args[1], resolve)
		if serr != nil {
			return fmt.Errorf("small: %w", serr)
		}

		if err := a.Store().UpdatePreferredModel(scope, config.SelectedModelTypeLarge, largeSel); err != nil {
			return fmt.Errorf("write large: %w", err)
		}
		if err := a.Store().UpdatePreferredModel(scope, config.SelectedModelTypeSmall, smallSel); err != nil {
			return fmt.Errorf("write small: %w", err)
		}

		fmt.Fprintf(os.Stderr, "set large = %s/%s%s in %s scope\n",
			largeSel.Provider, largeSel.Model, effortSuffix(largeSel.ReasoningEffort), scope)
		fmt.Fprintf(os.Stderr, "set small = %s/%s%s in %s scope\n",
			smallSel.Provider, smallSel.Model, effortSuffix(smallSel.ReasoningEffort), scope)
		return nil
	},
}

func effortSuffix(effort string) string {
	if effort == "" {
		return ""
	}
	return " effort=" + effort
}

func init() {
	modelsUseCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsUseCmd.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json)")
	modelsUseCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsCmd.AddCommand(modelsUseCmd)
}
