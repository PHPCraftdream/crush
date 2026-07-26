// Fork addition: `gemini-del` undoes `gemini-init` — removes the
// crush/crush-fallback Gemini CLI custom commands. See gemini_init.go for
// context.
package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
)

var geminiDelCmd = &cobra.Command{
	Use:   "gemini-del",
	Short: "Remove the crush/crush-fallback custom commands from Gemini CLI",
	Long: `Undo ` + "`crush gemini-init`" + `: remove the crush and crush-fallback custom
commands from Gemini CLI's commands directory.

Only files that carry our sentinel are removed — foreign .toml files with
the same name are left alone with a warning.

Default is --global (~/.gemini/commands/). Use --local (or --cwd, which
implies it) to target the current project's .gemini/commands/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.gemini/commands/) — the default
crush gemini-del

# Remove from the current project instead
crush gemini-del --local

# Scope to another project (implies --local)
crush gemini-del --cwd /path/to/project
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
			commandsDir, err = resolveGeminiCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = resolveGeminiCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeGeminiCommands(commandsDir)
	},
}

// runGeminiDel is kept for tests that call it directly (local mode only).
func runGeminiDel(cwd string) error {
	commandsDir, err := resolveGeminiCommandsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeGeminiCommands(commandsDir)
}

// removeGeminiCommands removes both the crush and crush-fallback custom
// commands from commandsDir.
func removeGeminiCommands(commandsDir string) error {
	if err := removeSentinelledFile(filepath.Join(commandsDir, "crush.toml"), geminiSlashCommandSentinel); err != nil {
		return err
	}
	return removeSentinelledFile(filepath.Join(commandsDir, "crush-fallback.toml"), geminiSlashCommandSentinel)
}

func init() {
	geminiDelCmd.Flags().Bool("global", false, "Remove from ~/.gemini/commands/. Default when neither --global nor --local is given.")
	geminiDelCmd.Flags().Bool("local", false, "Remove from the current project's .gemini/commands/ instead of ~/.gemini/commands/.")
	rootCmd.AddCommand(geminiDelCmd)
}
