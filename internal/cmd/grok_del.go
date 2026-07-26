// Fork addition: `grok-del` undoes `grok-init` — removes the
// crush/crush-fallback Grok Build CLI Skills. See grok_init.go for context.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var grokDelCmd = &cobra.Command{
	Use:   "grok-del",
	Short: "Remove the crush/crush-fallback Skills from Grok Build CLI",
	Long: `Undo ` + "`crush grok-init`" + `: remove the crush and crush-fallback Skills
from Grok Build CLI's Skills directory.

Only Skills that carry our sentinel are removed — foreign SKILL.md files
with the same name are left alone with a warning.

Default is --global (~/.grok/skills/). Use --local (or --cwd, which
implies it) to target the current project's .grok/skills/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.grok/skills/) — the default
crush grok-del

# Remove from the current project instead
crush grok-del --local

# Scope to another project (implies --local)
crush grok-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var skillsDir string
		if localMode {
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			skillsDir, err = resolveGrokSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = resolveGrokSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeGrokSkills(skillsDir)
	},
}

// runGrokDel is kept for tests that call it directly (local mode only).
func runGrokDel(cwd string) error {
	skillsDir, err := resolveGrokSkillsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeGrokSkills(skillsDir)
}

// removeGrokSkills removes both the crush and crush-fallback Skills from
// skillsDir.
func removeGrokSkills(skillsDir string) error {
	if err := removeSentinelledSkillDir(skillsDir, "crush", claudeSlashCommandSentinel); err != nil {
		return err
	}
	return removeSentinelledSkillDir(skillsDir, "crush-fallback", claudeSlashCommandSentinel)
}

func init() {
	grokDelCmd.Flags().Bool("global", false, "Remove from ~/.grok/skills/. Default when neither --global nor --local is given.")
	grokDelCmd.Flags().Bool("local", false, "Remove from the current project's .grok/skills/ instead of ~/.grok/skills/.")
	rootCmd.AddCommand(grokDelCmd)
}
