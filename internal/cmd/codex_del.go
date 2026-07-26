// Fork addition: `codex-del` undoes `codex-init` — removes the
// crush/crush-fallback Codex CLI Skills. See codex_init.go for context.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var codexDelCmd = &cobra.Command{
	Use:   "codex-del",
	Short: "Remove the crush/crush-fallback Skills from Codex CLI",
	Long: `Undo ` + "`crush codex-init`" + `: remove the crush and crush-fallback Skills
from Codex CLI's Skills directory.

Only Skills that carry our sentinel are removed — foreign SKILL.md files
with the same name are left alone with a warning.

Default is --global (~/.agents/skills/). Use --local (or --cwd, which
implies it) to target the current project's .agents/skills/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.agents/skills/) — the default
crush codex-del

# Remove from the current project instead
crush codex-del --local

# Scope to another project (implies --local)
crush codex-del --cwd /path/to/project
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
			skillsDir, err = resolveCodexSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = resolveCodexSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeCodexSkills(skillsDir)
	},
}

// runCodexDel is kept for tests that call it directly (local mode only).
func runCodexDel(cwd string) error {
	skillsDir, err := resolveCodexSkillsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeCodexSkills(skillsDir)
}

// removeCodexSkills removes both the crush and crush-fallback Skills from
// skillsDir.
func removeCodexSkills(skillsDir string) error {
	if err := removeSentinelledSkillDir(skillsDir, "crush", claudeSlashCommandSentinel); err != nil {
		return err
	}
	return removeSentinelledSkillDir(skillsDir, "crush-fallback", claudeSlashCommandSentinel)
}

func init() {
	codexDelCmd.Flags().Bool("global", false, "Remove from ~/.agents/skills/. Default when neither --global nor --local is given.")
	codexDelCmd.Flags().Bool("local", false, "Remove from the current project's .agents/skills/ instead of ~/.agents/skills/.")
	rootCmd.AddCommand(codexDelCmd)
}
