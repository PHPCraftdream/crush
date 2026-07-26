// Fork patch: `crush models bump <role> <up|down>` — step a role's reasoning
// effort by exactly one level in its model's own ordered Levels() array, and
// show the before/after effort so the effect is visible without a separate
// `models state` call.
//
// NAMING: this is deliberately NOT called `crush models effort` (singular).
// `crush models efforts` (plural, task #69/#71) already exists as a
// documentation/discoverability command ("what levels does this model
// support and how do I set them"). "effort" vs "efforts" differs by exactly
// one character and is exactly the kind of thing a user or a scripted caller
// mistypes; picking a distinct verb ("bump") removes that ambiguity entirely
// instead of relying on people reading the plural carefully. `bump <role>
// up|down` also reads naturally as an imperative action, matching `models
// use`/`models unset`'s verb-first style.
package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var modelsBumpCmd = &cobra.Command{
	Use:   "bump <large|small|worker|reviewer> <up|down>",
	Short: "Step a role's reasoning effort up or down by one level",
	Long: `Step a role's effort one level up or down and print the before/after
value. Unlike ` + "`crush models efforts [model]`" + ` (which only shows what levels
are available), ` + "`bump`" + ` writes the new value.

` + "`up`" + ` from unset lands on the lowest level; ` + "`down`" + ` from unset reports
nothing to lower. At either end of the list, the command prints a message
and exits 0 (not an error).

Scoped like ` + "`crush models use`" + ` (` + "`--global`" + `/` + "`--local`" + `).`,
	Args: cobra.ExactArgs(2),
	Example: `
# Step by step through a GLM-5.2 role: off -> high -> max.
crush models bump reviewer up    # off -> high
crush models bump reviewer up    # high -> max
crush models bump reviewer up    # already at max, prints a message, exits 0

crush models bump worker down    # one level down
crush models bump --local large down   # workspace-scoped

crush models state   # see the full picture afterward
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		roleArg, dirArg := args[0], args[1]

		slot, err := parseBumpRole(roleArg)
		if err != nil {
			return err
		}
		up, err := parseBumpDirection(dirArg)
		if err != nil {
			return err
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

		cfg := a.Config()
		current, has := cfg.Models[slot]
		if !has {
			fmt.Fprintf(os.Stderr, "%s is not set in any scope — nothing to bump. Set it first with `crush models use`.\n", roleArg)
			return nil
		}

		atomKey := lookupAtomForModel(current)
		if atomKey == "" {
			fmt.Fprintf(os.Stderr,
				"%s (%s/%s) is not a known atom — stepping isn't supported for this model.\n"+
					"See `crush models efforts %s/%s` or set an explicit level manually via\n"+
					"`crush models use ... --%s %s/%s@<level>`.\n",
				roleArg, current.Provider, current.Model, current.Provider, current.Model, roleArg, current.Provider, current.Model)
			return nil
		}

		levels := atomRegistry[atomKey].Levels()
		if levels == nil {
			fmt.Fprintf(os.Stderr,
				"%s (atom: %s) declares no effort levels — stepping isn't supported for this model.\n"+
					"See `crush models efforts %s`.\n",
				roleArg, atomKey, atomKey)
			return nil
		}

		// Index of the current effort within levels. An unset/empty effort
		// (current.ReasoningEffort == "") is treated as index -1 — i.e.
		// conceptually one position BELOW levels[0]. This makes both
		// directions behave sensibly without a special-cased branch per
		// direction:
		//   up   from unset (-1) lands on levels[0]      (the lowest level)
		//   down from unset (-1) has nowhere lower to go -> boundary message
		// The alternative (treating unset as "already at some real level",
		// e.g. defaulting it to the middle of the array) would require
		// guessing which level unset behaves like on the wire, which is
		// itself a provider-specific fact (see zaiReasoningLevels' doc comment:
		// Z.AI's own default differs from the fork's coordinator.go default).
		// Modeling unset as "below the bottom" sidesteps that guess entirely
		// and gives a single, predictable rule for every provider.
		idx := -1
		for i, l := range levels {
			if l == current.ReasoningEffort {
				idx = i
				break
			}
		}
		if idx == -1 && current.ReasoningEffort != "" {
			// The stored effort isn't in this atom's own Levels() at all
			// (e.g. a hand-edited config, or an atom whose Levels() changed
			// since the value was written). Don't guess a position — report
			// it plainly rather than silently treating it as "unset".
			fmt.Fprintf(os.Stderr,
				"%s is currently %q, which is not one of %s's known levels (%s) — can't determine a step direction.\n"+
					"Set an explicit valid level first via `crush models use`.\n",
				roleArg, current.ReasoningEffort, atomKey, strings.Join(levels, "|"))
			return nil
		}

		var newIdx int
		if up {
			newIdx = idx + 1
			if newIdx >= len(levels) {
				fmt.Fprintf(os.Stderr, "%s is already at the highest level (%s) for %s/%s — nothing to raise.\n",
					roleArg, levels[len(levels)-1], current.Provider, current.Model)
				return nil
			}
		} else {
			newIdx = idx - 1
			if newIdx < 0 {
				if idx == -1 {
					fmt.Fprintf(os.Stderr, "%s is already at the lowest level (unset) for %s/%s — nothing to lower.\n",
						roleArg, current.Provider, current.Model)
				} else {
					fmt.Fprintf(os.Stderr, "%s is already at the lowest level (%s) for %s/%s — nothing to lower.\n",
						roleArg, levels[0], current.Provider, current.Model)
				}
				return nil
			}
		}

		newEffort := levels[newIdx]
		updated := current
		updated.ReasoningEffort = newEffort

		if err := a.Store().UpdatePreferredModels(scope, map[config.SelectedModelType]config.SelectedModel{
			slot: updated,
		}); err != nil {
			return fmt.Errorf("write %s: %w", roleArg, err)
		}

		oldLabel := current.ReasoningEffort
		if oldLabel == "" {
			oldLabel = "(unset)"
		}
		fmt.Fprintf(os.Stderr, "%s: %s -> %s (%s/%s, %s scope)\n",
			roleArg, oldLabel, newEffort, current.Provider, current.Model, scope)
		return nil
	},
}

// parseBumpRole validates and maps the role positional to a SelectedModelType.
func parseBumpRole(s string) (config.SelectedModelType, error) {
	switch s {
	case "large":
		return config.SelectedModelTypeLarge, nil
	case "small":
		return config.SelectedModelTypeSmall, nil
	case "worker":
		return config.SelectedModelTypeWorker, nil
	case "reviewer":
		return config.SelectedModelTypeReviewer, nil
	default:
		return "", fmt.Errorf("unexpected role %q — expected large|small|worker|reviewer", s)
	}
}

// parseBumpDirection validates the up/down positional.
func parseBumpDirection(s string) (up bool, err error) {
	switch s {
	case "up":
		return true, nil
	case "down":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected direction %q — expected up|down", s)
	}
}

func init() {
	modelsBumpCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsBumpCmd.Flags().Bool("local", false, "Target the workspace config (./.crush/crush.json)")
	modelsBumpCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsCmd.AddCommand(modelsBumpCmd)
}
