// Fork patch: batch 13 — `crush models unset [large|small|both] [--global|--local]`
// removes a model override from the chosen scope so the other scope takes
// effect, without having to hand-edit crush.json or `rm` the whole file.
package cmd

import (
	"fmt"
	"os"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var modelsUnsetCmd = &cobra.Command{
	Use:   "unset [large|small|worker|reviewer|both|all]",
	Short: "Remove a model override from the chosen scope (defaults to large+small, global scope)",
	Long: `Delete the models.<slot> entry (or entries) from the chosen scope's
crush.json so the OTHER scope's value becomes effective again.

Positional arg (optional):
  large     — only the large slot
  small     — only the small slot
  worker    — only the optional worker slot
  reviewer  — only the optional reviewer slot
  both      — large + small (default if omitted; matches ` + "`crush models use`" + `'s scope)
  all       — all four slots, including worker/reviewer

Scope flags (mutually exclusive):
  --global  (default) ~/.local/share/crush/crush.json
  --local             ./.crush/crush.json

Missing keys are a no-op (exit 0). After the deletion, an empty
"models" object is also stripped so the file stays clean.`,
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: []string{"large", "small", "worker", "reviewer", "both", "all"},
	Example: `
# Clear the large+small workspace override so the global config takes effect again.
crush models unset --local

# Same but globally — wipes large+small from ~/.local/share/crush/crush.json.
crush models unset --global

# Drop just the large slot in the workspace; keep the small one.
crush models unset large --local

# Drop just the small slot globally.
crush models unset small --global

# Clear the worker slot globally (falls back to no worker — sub-agents use large).
crush models unset worker --global

# Clear the reviewer slot in the workspace.
crush models unset reviewer --local

# Clear all four slots (large, small, worker, reviewer) globally.
crush models unset all --global

# Confirm what survived:
crush models state
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		which := "both"
		if len(args) == 1 {
			which = args[0]
		}
		switch which {
		case "large", "small", "worker", "reviewer", "both", "all":
			// ok
		default:
			return fmt.Errorf("unexpected positional %q — expected large|small|worker|reviewer|both|all", which)
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

		store := a.Store()

		// Snapshot prior values so we can show what was unset.
		priorAll, _ := store.ReadAllModelsAtScope(scope)

		targets := []struct {
			label string
			key   string
			prior *config.SelectedModel
		}{
			{"large", "models.large", priorAll[config.SelectedModelTypeLarge]},
			{"small", "models.small", priorAll[config.SelectedModelTypeSmall]},
			{"worker", "models.worker", priorAll[config.SelectedModelTypeWorker]},
			{"reviewer", "models.reviewer", priorAll[config.SelectedModelTypeReviewer]},
		}

		// "both" (the default) only ever touched large+small; "all" is the new
		// spelling for every slot including worker/reviewer.
		selected := func(label string) bool {
			switch which {
			case "both":
				return label == "large" || label == "small"
			case "all":
				return true
			default:
				return which == label
			}
		}

		didDelete := false
		for _, t := range targets {
			if !selected(t.label) {
				continue
			}
			if t.prior == nil {
				fmt.Fprintf(os.Stderr, "%s was not set in %s scope (no-op)\n", t.label, scope)
				continue
			}
			if err := store.RemoveConfigField(scope, t.key); err != nil {
				return fmt.Errorf("failed to unset %s in %s scope: %w", t.label, scope, err)
			}
			fmt.Fprintf(os.Stderr, "unset %s in %s scope (was %s/%s%s)\n",
				t.label, scope, t.prior.Provider, t.prior.Model, effortSuffix(t.prior.ReasoningEffort))
			didDelete = true
		}

		// If we just emptied the `models` object, strip it so the scope file
		// doesn't sit as `"models": {}`. Best-effort: if the read or write
		// fails, do not surface the error — the field-level unset already
		// succeeded and that's what the user asked for.
		if didDelete {
			postAll, perr := store.ReadAllModelsAtScope(scope)
			if perr == nil && len(postAll) == 0 {
				_ = store.RemoveConfigField(scope, "models")
			}
		}

		return nil
	},
}

func init() {
	modelsUnsetCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsUnsetCmd.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json)")
	modelsUnsetCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsCmd.AddCommand(modelsUnsetCmd)
}
