package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var modelsPresetCmd = &cobra.Command{
	Use:                "preset",
	Hidden:             true,
	Short:              "(removed — use `crush models use` / `crush models list`)",
	DisableFlagParsing: true, // print the redirect even when caller passes legacy preset args.
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr,
			"`crush models preset` was removed in batch 11.\n"+
				"Use `crush models list` to see atoms, then `crush models use <large> <small>`.")
		os.Exit(2)
	},
}

func init() {
	modelsCmd.AddCommand(modelsPresetCmd)
}
