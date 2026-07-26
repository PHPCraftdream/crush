// Fork patch: batch 11 — `crush models state` shows the effective large/small
// pair, the scope each came from, and a per-scope breakdown of what is written
// to disk. Replaces the implicit story (`models show` alone doesn't say WHERE
// each slot came from).
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var modelsStateCmd = &cobra.Command{
	Use:     "state",
	Aliases: []string{"show"}, // backwards-compat: `crush models show` used to exist.
	Short:   "Show what's currently effective and from which scope",
	Long: `Print three things:
  1. EFFECTIVE — the (large, small, worker, reviewer) values that
     ` + "`crush run --role smart/fast/worker/reviewer`" + ` will actually use, and
     which scope each came from. worker and reviewer are optional; when unset
     in both scopes they print "(not set in any scope)".
  2. SCOPES — what each scope (global, local) says about each slot, with
     "(effective)" / "(overridden by local)" / "(not set)" annotations.
  3. The atom name in parens when the effective model matches a known atom.
  4. For a slot with no explicit effort, the known unset-default (e.g.
     "unset -> thinking on, high" for Z.AI) — silent when undocumented.

Set worker/reviewer with ` + "`crush models use <large> <small> --worker <m> --reviewer <m>`" + `
and clear them with ` + "`crush models unset worker`" + ` / ` + "`crush models unset reviewer`" + `.

` + "`--json`" + ` emits a structured object for orchestrators.`,
	Example: `
# Plain text: effective pair + scope breakdown.
crush models state

# Machine-readable for orchestrators (jq-friendly):
crush models state --json | jq '.effective'

# After changing the workspace override, see what's now effective:
crush models use --local opus-high glm5_turbo && crush models state

# Backwards-compat alias of the same command:
crush models show
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		cfg := a.Config()
		store := a.Store()

		globalAll, gerr := store.ReadAllModelsAtScope(config.ScopeGlobal)
		if gerr != nil {
			return fmt.Errorf("read global scope: %w", gerr)
		}
		localAll, lerr := store.ReadAllModelsAtScope(config.ScopeWorkspace)
		if lerr != nil {
			return fmt.Errorf("read local scope: %w", lerr)
		}

		globalLarge, globalSmall := globalAll[config.SelectedModelTypeLarge], globalAll[config.SelectedModelTypeSmall]
		localLarge, localSmall := localAll[config.SelectedModelTypeLarge], localAll[config.SelectedModelTypeSmall]
		globalWorker, globalReviewer := globalAll[config.SelectedModelTypeWorker], globalAll[config.SelectedModelTypeReviewer]
		localWorker, localReviewer := localAll[config.SelectedModelTypeWorker], localAll[config.SelectedModelTypeReviewer]

		effLarge, hasLarge := cfg.Models[config.SelectedModelTypeLarge]
		effSmall, hasSmall := cfg.Models[config.SelectedModelTypeSmall]
		effWorker, hasWorker := cfg.Models[config.SelectedModelTypeWorker]
		effReviewer, hasReviewer := cfg.Models[config.SelectedModelTypeReviewer]

		largeScope := whichScope(localLarge, globalLarge)
		smallScope := whichScope(localSmall, globalSmall)
		workerScope := whichScope(localWorker, globalWorker)
		reviewerScope := whichScope(localReviewer, globalReviewer)

		if asJSON {
			payload := map[string]any{
				"effective": map[string]any{
					"large":                   nilOrModel(hasLarge, effLarge),
					"small":                   nilOrModel(hasSmall, effSmall),
					"worker":                  nilOrModel(hasWorker, effWorker),
					"reviewer":                nilOrModel(hasReviewer, effReviewer),
					"large_scope":             largeScope,
					"small_scope":             smallScope,
					"worker_scope":            workerScope,
					"reviewer_scope":          reviewerScope,
					"large_effort_default":    nilOrEffortDefault(hasLarge, effLarge),
					"small_effort_default":    nilOrEffortDefault(hasSmall, effSmall),
					"worker_effort_default":   nilOrEffortDefault(hasWorker, effWorker),
					"reviewer_effort_default": nilOrEffortDefault(hasReviewer, effReviewer),
				},
				"global": map[string]any{
					"large":    globalLarge,
					"small":    globalSmall,
					"worker":   globalWorker,
					"reviewer": globalReviewer,
				},
				"local": map[string]any{
					"large":    localLarge,
					"small":    localSmall,
					"worker":   localWorker,
					"reviewer": localReviewer,
				},
			}
			return json.NewEncoder(os.Stdout).Encode(payload)
		}

		fmt.Println("EFFECTIVE")
		printEffectiveLine("large", hasLarge, effLarge, largeScope)
		printEffectiveLine("small", hasSmall, effSmall, smallScope)
		printEffectiveLine("worker", hasWorker, effWorker, workerScope)
		printEffectiveLine("reviewer", hasReviewer, effReviewer, reviewerScope)
		fmt.Println()
		fmt.Println("SCOPES")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		printScopeLine(tw, "global", "large", globalLarge, localLarge, "global")
		printScopeLine(tw, "global", "small", globalSmall, localSmall, "global")
		printScopeLine(tw, "global", "worker", globalWorker, localWorker, "global")
		printScopeLine(tw, "global", "reviewer", globalReviewer, localReviewer, "global")
		printScopeLine(tw, "local", "large", localLarge, globalLarge, "local")
		printScopeLine(tw, "local", "small", localSmall, globalSmall, "local")
		printScopeLine(tw, "local", "worker", localWorker, globalWorker, "local")
		printScopeLine(tw, "local", "reviewer", localReviewer, globalReviewer, "local")
		tw.Flush()
		return nil
	},
}

func whichScope(local, global *config.SelectedModel) string {
	if local != nil {
		return "local"
	}
	if global != nil {
		return "global"
	}
	return ""
}

func nilOrModel(has bool, m config.SelectedModel) any {
	if !has {
		return nil
	}
	return m
}

// nilOrEffortDefault is the JSON counterpart of effortEffectiveNote: null
// when the slot is unset, when the slot's effort is explicitly set (nothing
// to report — the model's own reasoning_effort field already says so), or
// when the provider's unset-default behavior isn't documented. Otherwise the
// same short fact effortEffectiveNote renders in text (without the
// parens/leading space, since JSON consumers don't need the human framing).
func nilOrEffortDefault(has bool, m config.SelectedModel) any {
	if !has || m.ReasoningEffort != "" {
		return nil
	}
	if note := unsetEffortNote(m.Provider); note != "" {
		return note
	}
	return nil
}

func printEffectiveLine(label string, has bool, m config.SelectedModel, scope string) {
	if !has {
		fmt.Printf("  %s:  (not set in any scope)\n", label)
		return
	}
	atomLabel := ""
	if k := lookupAtomForModel(m); k != "" {
		if m.ReasoningEffort != "" {
			atomLabel = fmt.Sprintf(" (atom: %s-%s)", k, m.ReasoningEffort)
		} else {
			atomLabel = fmt.Sprintf(" (atom: %s)", k)
		}
	}
	src := scope
	switch scope {
	case "global":
		src = "from GLOBAL"
	case "local":
		src = "from LOCAL"
	default:
		src = "scope unknown"
	}
	fmt.Printf("  %s:  %s/%s%s%s%s   (%s)\n",
		label, m.Provider, m.Model, effortSuffix(m.ReasoningEffort), atomLabel, effortEffectiveNote(m), src)
}

// effortEffectiveNote returns a terse parenthetical (with a leading space,
// or "" when there's nothing to add) for a slot's effort state: nothing
// when an effort is explicitly set (effortSuffix above already shows that),
// and the documented unset-default fact — reusing unsetEffortNote from
// models_efforts.go so this can never drift from `crush models efforts`'s
// prose or coordinator.go's actual switch — when the effort is unset and
// that provider's default is known. Silent ("") for unset effort on a
// provider whose default isn't documented; never guesses.
func effortEffectiveNote(m config.SelectedModel) string {
	if m.ReasoningEffort != "" {
		return ""
	}
	if note := unsetEffortNote(m.Provider); note != "" {
		return "  (" + note + ")"
	}
	return ""
}

func printScopeLine(tw *tabwriter.Writer, scopeName, slot string, value, other *config.SelectedModel, ownScope string) {
	if value == nil {
		fmt.Fprintf(tw, "  %s\t%s = —\t(not set)\n", scopeName, slot)
		return
	}
	annotation := ""
	switch {
	case ownScope == "global" && other != nil:
		annotation = "(overridden by local)"
	case ownScope == "global":
		annotation = "(effective)"
	case ownScope == "local":
		annotation = "(effective)"
	}
	fmt.Fprintf(tw, "  %s\t%s = %s/%s%s%s\t%s\n",
		scopeName, slot, value.Provider, value.Model, effortSuffix(value.ReasoningEffort), effortEffectiveNote(*value), annotation)
}

func init() {
	modelsStateCmd.Flags().Bool("json", false, "Emit a structured JSON object")
	modelsCmd.AddCommand(modelsStateCmd)
}
