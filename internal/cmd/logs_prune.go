package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var logsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Truncate the crush log file to zero bytes",
	Long: `Truncate .crush/logs/crush.log to reclaim disk space.

crush does not auto-rotate its log file — on busy workspaces it can
grow to hundreds of megabytes. This command blanks it atomically (the
same way logrotate's copytruncate works) so running sessions keep
appending without a reopen.`,
	Example: `
crush logs prune
crush logs path    # check size before/after
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, _ := cmd.Flags().GetString("cwd")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		cfg, err := config.Load(cwd, dataDir, false)
		if err != nil {
			return err
		}
		p := filepath.Join(cfg.Config().Options.DataDirectory, "logs", "crush.log")
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "(no log file)")
				return nil
			}
			return err
		}
		before := info.Size()
		if err := os.Truncate(p, 0); err != nil {
			return fmt.Errorf("truncate %s: %w", p, err)
		}
		fmt.Fprintf(os.Stderr, "pruned %s (%d bytes → 0)\n", p, before)
		return nil
	},
}
