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
  1. EFFECTIVE — the (large, small) pair that ` + "`crush run --role smart/fast`" + `
     will actually use, and which scope each came from.
  2. SCOPES — what each scope (global, local) says about each slot, with
     "(effective)" / "(overridden by local)" / "(not set)" annotations.
  3. The atom name in parens when the effective model matches a known atom.

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

		globalLarge, globalSmall, gerr := store.ReadModelsAtScope(config.ScopeGlobal)
		if gerr != nil {
			return fmt.Errorf("read global scope: %w", gerr)
		}
		localLarge, localSmall, lerr := store.ReadModelsAtScope(config.ScopeWorkspace)
		if lerr != nil {
			return fmt.Errorf("read local scope: %w", lerr)
		}

		effLarge, hasLarge := cfg.Models[config.SelectedModelTypeLarge]
		effSmall, hasSmall := cfg.Models[config.SelectedModelTypeSmall]

		largeScope := whichScope(localLarge, globalLarge)
		smallScope := whichScope(localSmall, globalSmall)

		if asJSON {
			payload := map[string]any{
				"effective": map[string]any{
					"large":       nilOrModel(hasLarge, effLarge),
					"small":       nilOrModel(hasSmall, effSmall),
					"large_scope": largeScope,
					"small_scope": smallScope,
				},
				"global": map[string]any{
					"large": globalLarge,
					"small": globalSmall,
				},
				"local": map[string]any{
					"large": localLarge,
					"small": localSmall,
				},
			}
			return json.NewEncoder(os.Stdout).Encode(payload)
		}

		fmt.Println("EFFECTIVE")
		printEffectiveLine("large", hasLarge, effLarge, largeScope)
		printEffectiveLine("small", hasSmall, effSmall, smallScope)
		fmt.Println()
		fmt.Println("SCOPES")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		printScopeLine(tw, "global", "large", globalLarge, localLarge, "global")
		printScopeLine(tw, "global", "small", globalSmall, localSmall, "global")
		printScopeLine(tw, "local", "large", localLarge, globalLarge, "local")
		printScopeLine(tw, "local", "small", localSmall, globalSmall, "local")
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
	fmt.Printf("  %s:  %s/%s%s%s   (%s)\n",
		label, m.Provider, m.Model, effortSuffix(m.ReasoningEffort), atomLabel, src)
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
	fmt.Fprintf(tw, "  %s\t%s = %s/%s%s\t%s\n",
		scopeName, slot, value.Provider, value.Model, effortSuffix(value.ReasoningEffort), annotation)
}

func init() {
	modelsStateCmd.Flags().Bool("json", false, "Emit a structured JSON object")
	modelsCmd.AddCommand(modelsStateCmd)
}
