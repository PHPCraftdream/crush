package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var logsPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the path to the crush log file",
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
			fmt.Println(p, "(does not exist)")
		} else {
			fmt.Printf("%s (%d bytes)\n", p, info.Size())
		}
		return nil
	},
}
