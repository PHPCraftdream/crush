// Fork patch: batch 11 — `crush models set` removed in favour of
// `crush models use <large> <small>`. This file keeps only:
//   1. A hidden cobra command that prints a redirect notice + exits 2.
//   2. `splitModelEffort`, the @level-suffix helper still used by atom parsing
//      and a couple of tests.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var modelsSetCmd = &cobra.Command{
	Use:                "set",
	Hidden:             true,
	Short:              "(removed — use `crush models use`)",
	DisableFlagParsing: true, // print the redirect even when caller passes --large/--small/etc.
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr,
			"`crush models set` was removed in batch 11.\n"+
				"Use `crush models use <large> <small>` instead. See `crush models list` for atoms.")
		os.Exit(2)
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
	// `models show` flag registration still belongs here for backwards
	// compat — the command itself lives in models_show.go (was inlined
	// into the old models_set.go before batch 11; if absent, this is a
	// no-op).
	modelsCmd.AddCommand(modelsSetCmd)
}
