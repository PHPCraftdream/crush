// Fork addition: `qwen-del` undoes `qwen-init` — removes the
// crush/crush-fallback Qwen Code CLI slash-commands. See qwen_init.go
// for context.
package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
)

var qwenDelCmd = &cobra.Command{
	Use:   "qwen-del",
	Short: "Remove the crush/crush-fallback slash-commands from Qwen Code CLI",
	Long: `Undo ` + "`crush qwen-init`" + `: remove the crush and crush-fallback commands
from Qwen Code CLI's commands directory.

Only files that carry our sentinel are removed — foreign files with the
same name are left alone with a warning.

Default is --global (~/.qwen/commands/). Use --local (or --cwd, which
implies it) to target the current project's .qwen/commands/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.qwen/commands/) — the default
crush qwen-del

# Remove from the current project instead
crush qwen-del --local

# Scope to another project (implies --local)
crush qwen-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var commandsDir string
		if localMode {
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			commandsDir, err = resolveQwenCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = resolveQwenCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeQwenCommands(commandsDir)
	},
}

// runQwenDel is kept for tests that call it directly (local mode only).
func runQwenDel(cwd string) error {
	commandsDir, err := resolveQwenCommandsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeQwenCommands(commandsDir)
}

// removeQwenCommands removes both the crush and crush-fallback commands
// from commandsDir.
func removeQwenCommands(commandsDir string) error {
	if err := removeSentinelledFile(filepath.Join(commandsDir, "crush.md"), claudeSlashCommandSentinel); err != nil {
		return err
	}
	return removeSentinelledFile(filepath.Join(commandsDir, "crush-fallback.md"), claudeSlashCommandSentinel)
}

func init() {
	qwenDelCmd.Flags().Bool("global", false, "Remove from ~/.qwen/commands/. Default when neither --global nor --local is given.")
	qwenDelCmd.Flags().Bool("local", false, "Remove from the current project's .qwen/commands/ instead of ~/.qwen/commands/.")
	rootCmd.AddCommand(qwenDelCmd)
}
