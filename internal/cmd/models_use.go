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
	Short: "Set the large and small slots (and optionally worker/reviewer) from atom names",
	Long: `Activate a (large, small) pair using the atom syntax. Each argument is
either an atom name (e.g. "opus-high", "glm5_turbo") OR a raw
"provider/model[@level]" string for models not in the atom registry.

The chosen scope is written to crush.json:
  --global (default)  ~/.local/share/crush/crush.json
  --local             ./.crush/crush.json

The current value in the OTHER scope is preserved; effective resolution
remains "local if set, else global".

The two positional args always set the large ("smart") and small ("fast")
slots. The optional worker and reviewer slots (see ` + "`crush models --help`" + `
for what each is for) are set with ` + "`--worker`" + ` / ` + "`--reviewer`" + ` — same atom
or "provider/model[@level]" syntax, resolved and written independently of
large/small. Omit a flag to leave that slot untouched.

See ` + "`crush models list`" + ` for the full atom table.`,
	Args: cobra.ExactArgs(2),
	Example: `
# Short codes: Opus 4.7 xhigh (1M ctx) + Haiku 4.5 low (200k ctx)
crush models use o47x h45l

# Sonnet 4.6 high (200k ctx) + Haiku 4.5 low — cheaper than Opus, still smart
crush models use s46h h45l

# Max thinking on large (1M ctx), fast on small
crush models use o47xx h45l

# Z.AI stack
crush models use glm5_1 glm5_turbo

# Mixed: Opus xhigh (1M ctx) + Z.AI turbo
crush models use o47x glm5_turbo

# Long-form atom syntax still works
crush models use opus-high sonnet-low

# Also set the worker slot (cheap sub-agent model) in the same call
crush models use o47x h45l --worker glm5_turbo

# Also set the reviewer slot (strongest model, --role reviewer only)
crush models use o47x h45l --reviewer oxx

# Set worker and reviewer together with large/small
crush models use o47x h45l --worker fl --reviewer oxx

# Workspace-only override (writes ./.crush/crush.json, leaves global untouched).
crush models use --local o47x h45l

# Raw "provider/model[@level]" syntax for models not in the registry.
crush models use openai/gpt-5@high zai/glm-5-turbo

# After running, verify with:
crush models state
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		workerArg, _ := cmd.Flags().GetString("worker")
		reviewerArg, _ := cmd.Flags().GetString("reviewer")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		resolve := func(modelPart string) (string, string, error) {
			provider, modelID, rerr := a.ResolveModel(modelPart)
			return provider, modelID, rerr
		}

		// Pass 1: parse + validate EVERY provided argument before writing
		// anything. This must have zero side effects — no config writes —
		// so that a validation failure on a later field (e.g. --reviewer)
		// can never leave an earlier field (large/small/--worker) already
		// persisted. See CLAUDE.md task tracking / bug report: previously
		// large and small were written immediately after being parsed, so a
		// bad --reviewer value failed the command AFTER large/small (and
		// worker) were already durably written to disk — a silent partial
		// write masquerading as a no-op failure.
		largeSel, lerr := parseAtomOrRaw(args[0], resolve)
		if lerr != nil {
			return fmt.Errorf("large: %w", lerr)
		}
		smallSel, serr := parseAtomOrRaw(args[1], resolve)
		if serr != nil {
			return fmt.Errorf("small: %w", serr)
		}

		var workerSel config.SelectedModel
		if workerArg != "" {
			var werr error
			workerSel, werr = parseAtomOrRaw(workerArg, resolve)
			if werr != nil {
				return fmt.Errorf("worker: %w", werr)
			}
		}

		var reviewerSel config.SelectedModel
		if reviewerArg != "" {
			var rerr error
			reviewerSel, rerr = parseAtomOrRaw(reviewerArg, resolve)
			if rerr != nil {
				return fmt.Errorf("reviewer: %w", rerr)
			}
		}

		// Pass 2: every provided argument validated successfully — now, and
		// only now, write. All slots are written in a single call to
		// UpdatePreferredModels, which batches them into one SetConfigFields
		// write (one atomicWriteFile) instead of one write per slot — so
		// there's no window, even for an I/O-level failure, where only some
		// of the provided slots landed on disk. This reuses the same
		// batch-write primitive `crush providers patch` already relies on
		// (config.ConfigStore.SetConfigFields), rather than inventing a new
		// mechanism.
		toWrite := map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: largeSel,
			config.SelectedModelTypeSmall: smallSel,
		}
		if workerArg != "" {
			toWrite[config.SelectedModelTypeWorker] = workerSel
		}
		if reviewerArg != "" {
			toWrite[config.SelectedModelTypeReviewer] = reviewerSel
		}

		if err := a.Store().UpdatePreferredModels(scope, toWrite); err != nil {
			return fmt.Errorf("write models: %w", err)
		}

		fmt.Fprintf(os.Stderr, "set large = %s/%s%s in %s scope\n",
			largeSel.Provider, largeSel.Model, effortSuffix(largeSel.ReasoningEffort), scope)
		fmt.Fprintf(os.Stderr, "set small = %s/%s%s in %s scope\n",
			smallSel.Provider, smallSel.Model, effortSuffix(smallSel.ReasoningEffort), scope)
		if workerArg != "" {
			fmt.Fprintf(os.Stderr, "set worker = %s/%s%s in %s scope\n",
				workerSel.Provider, workerSel.Model, effortSuffix(workerSel.ReasoningEffort), scope)
		}
		if reviewerArg != "" {
			fmt.Fprintf(os.Stderr, "set reviewer = %s/%s%s in %s scope\n",
				reviewerSel.Provider, reviewerSel.Model, effortSuffix(reviewerSel.ReasoningEffort), scope)
		}

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
	modelsUseCmd.Flags().String("worker", "", "Also set the optional worker slot (atom or provider/model[@level])")
	modelsUseCmd.Flags().String("reviewer", "", "Also set the optional reviewer slot (atom or provider/model[@level])")
	modelsCmd.AddCommand(modelsUseCmd)
}
