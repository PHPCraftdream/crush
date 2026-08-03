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
	Use:   "use [<large> <small>]",
	Short: "Set any of the four model slots from atom names — all at once or one at a time",
	Long: `Activate model slots using the atom syntax. Each value is either an atom
name (e.g. "opus-high", "glm5_turbo") OR a raw "provider/model[@level]"
string for models not in the atom registry.

The chosen scope is written to crush.json:
  --global (default)  ~/.local/share/crush/crush.json
  --local             ./.crush/crush.json

The current value in the OTHER scope is preserved; effective resolution
remains "local if set, else global".

Two forms are supported — never mix them in the same call:

  1. Positional (sets large + small together):
       crush models use <large> <small>

  2. Flags (set any subset of the four slots independently — e.g. only the
     fast/small model, leaving large/worker/reviewer untouched):
       crush models use --small <atom>
       crush models use --large <atom> --reviewer <atom>

--worker and --reviewer (see ` + "`crush models --help`" + ` for what each is for)
are ALWAYS flag-only, in both forms, and are resolved/written independently
of large/small. Omit a flag to leave that slot untouched.

Every one of the four slots accepts an effort suffix right here — either
"<atom>-<level>" (e.g. "glm5_2-max") or "provider/model@level" — no separate
step needed. See ` + "`crush models efforts [model]`" + ` to list the levels a model
supports, and ` + "`crush models bump <role> up|down`" + ` to nudge an already-set
effort later.

See ` + "`crush models list`" + ` for the full atom table.`,
	Args: cobra.MaximumNArgs(2),
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

# Set effort on a role slot in the same call: "<atom>-<level>" or "provider/model@level"
crush models use o47x h45l --reviewer glm5_2-max

# Set worker and reviewer together with large/small
crush models use o47x h45l --worker fl --reviewer oxx

# Workspace-only override (writes ./.crush/crush.json, leaves global untouched).
crush models use --local o47x h45l

# Raw "provider/model[@level]" syntax for models not in the registry.
crush models use openai/gpt-5@high zai/glm-5-turbo

# Change ONLY the fast/small model, leaving large/worker/reviewer untouched
crush models use --small glm4_7_flash

# Change ONLY the smart/large model
crush models use --large o47x

# --large and --small together, still without touching worker/reviewer
crush models use --large o47x --small h45l

# After running, verify with:
crush models state
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		largeFlag, _ := cmd.Flags().GetString("large")
		smallFlag, _ := cmd.Flags().GetString("small")
		workerArg, _ := cmd.Flags().GetString("worker")
		reviewerArg, _ := cmd.Flags().GetString("reviewer")

		// Exactly one of the two forms documented above may be used per
		// call: positional <large> <small>, or --large/--small flags. Never
		// both — a call with both would leave it ambiguous which value
		// actually wins, and silently preferring one over the other is
		// worse than just refusing.
		var largeArg, smallArg string
		switch len(args) {
		case 2:
			if largeFlag != "" || smallFlag != "" {
				return fmt.Errorf("cannot combine positional <large> <small> args with --large/--small flags — use one form or the other (see `crush models use --help`)")
			}
			largeArg, smallArg = args[0], args[1]
		case 0:
			largeArg, smallArg = largeFlag, smallFlag
		default:
			return fmt.Errorf("expected 0 positional args (use --large/--small to update individual slots) or exactly 2 (<large> <small>) — got %d", len(args))
		}

		if largeArg == "" && smallArg == "" && workerArg == "" && reviewerArg == "" {
			return fmt.Errorf("nothing to set — provide <large> <small> positionally, or at least one of --large/--small/--worker/--reviewer")
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

		// Pass 1: parse + validate EVERY provided argument before writing
		// anything. This must have zero side effects — no config writes —
		// so that a validation failure on a later field (e.g. --reviewer)
		// can never leave an earlier field (large/small/--worker) already
		// persisted. See CLAUDE.md task tracking / bug report: previously
		// large and small were written immediately after being parsed, so a
		// bad --reviewer value failed the command AFTER large/small (and
		// worker) were already durably written to disk — a silent partial
		// write masquerading as a no-op failure.
		var largeSel config.SelectedModel
		if largeArg != "" {
			var lerr error
			largeSel, lerr = parseAtomOrRaw(largeArg, resolve)
			if lerr != nil {
				return fmt.Errorf("large: %w", lerr)
			}
		}

		var smallSel config.SelectedModel
		if smallArg != "" {
			var serr error
			smallSel, serr = parseAtomOrRaw(smallArg, resolve)
			if serr != nil {
				return fmt.Errorf("small: %w", serr)
			}
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
		toWrite := map[config.SelectedModelType]config.SelectedModel{}
		if largeArg != "" {
			toWrite[config.SelectedModelTypeLarge] = largeSel
		}
		if smallArg != "" {
			toWrite[config.SelectedModelTypeSmall] = smallSel
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

		if largeArg != "" {
			fmt.Fprintf(os.Stderr, "set large = %s/%s%s in %s scope\n",
				largeSel.Provider, largeSel.Model, effortSuffix(largeSel.ReasoningEffort), scope)
		}
		if smallArg != "" {
			fmt.Fprintf(os.Stderr, "set small = %s/%s%s in %s scope\n",
				smallSel.Provider, smallSel.Model, effortSuffix(smallSel.ReasoningEffort), scope)
		}
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
	modelsUseCmd.Flags().String("large", "", "Set only the large (\"smart\") slot (atom or provider/model[@level]) — cannot combine with positional args")
	modelsUseCmd.Flags().String("small", "", "Set only the small (\"fast\") slot (atom or provider/model[@level]) — cannot combine with positional args")
	modelsUseCmd.Flags().String("worker", "", "Also set the optional worker slot (atom or provider/model[@level])")
	modelsUseCmd.Flags().String("reviewer", "", "Also set the optional reviewer slot (atom or provider/model[@level])")
	modelsCmd.AddCommand(modelsUseCmd)
}
