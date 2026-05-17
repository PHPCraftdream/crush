package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var claudePrintCmd = &cobra.Command{
	Use:   "claude-print",
	Short: "Print the claude-init guide block to stdout",
	Long: `Print the same block that ` + "`crush claude-init`" + ` would write into
CLAUDE.md, but to stdout instead of touching any file. Useful for:

  - inspecting the current text before running claude-init;
  - piping the block into another file ("crush claude-print > AGENTS.md");
  - feeding the guide into another LLM's system prompt as a one-off
    without committing anything to disk.

The block carries its versioned sentinel ("` + claudeInitMarkerStart + `" /
"` + claudeInitMarkerEnd + `") on stdout — strip them yourself if you don't
want them in the consumer.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprint(os.Stdout, claudeInitBlock())
		return err
	},
}

func init() {
	rootCmd.AddCommand(claudePrintCmd)
}
