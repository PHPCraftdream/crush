// Fork addition: `codex-init` installs the `crush`/`crush-fallback` slash
// commands as Codex CLI Skills (`.agents/skills/<name>/SKILL.md`). First of
// the `<tool>-init`/`<tool>-del` family alongside claude-init/claude-del;
// gemini-init/gemini-del, grok-init/grok-del and qwen-init/qwen-del follow
// the same pattern, converting from the same canonical source templates
// (claudeSlashCommandTemplate / claudeFallbackCommandTemplate, embedded in
// claude_init.go) via the helpers in multi_cli_convert.go.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const codexSkillsDir = ".agents/skills" // relative to cwd (local) or $HOME (global)

// resolveCodexSkillsDir returns the directory Codex CLI Skills should be
// written to. When global is true it returns ~/.agents/skills; otherwise
// <cwd>/.agents/skills. Mirrors resolveCommandsDir's shape (no repo-root
// search upward) but targets Codex's own convention.
func resolveCodexSkillsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, codexSkillsDir), nil
	}
	return filepath.Join(cwd, codexSkillsDir), nil
}

var codexInitCmd = &cobra.Command{
	Use:   "codex-init",
	Short: "Install the crush/crush-fallback Skills for Codex CLI",
	Long: `Set up crush's delegation Skills in Codex CLI.

Codex CLI Skills are written to ` + "`~/.agents/skills/crush/SKILL.md`" + ` and
` + "`~/.agents/skills/crush-fallback/SKILL.md`" + ` by default (the GLOBAL scope,
available in every project). Use --local (or --cwd, which implies it) to
scope them to the current project's ` + "`.agents/skills/`" + ` instead.

Content is converted from the same canonical source used by
` + "`claude-init`" + ` — the two stay in sync automatically.

Skipped (with a warning) if a target SKILL.md exists without our sentinel
— we never overwrite a file we don't own.`,
	Example: `
# Install / refresh the Codex Skills globally — the default
crush codex-init

# Install into the current project instead
crush codex-init --local

# Scope to another project (implies --local)
crush codex-init --cwd /path/to/project
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

		return installCodexSkills(skillsDir)
	},
}

// installCodexSkills writes both the crush and crush-fallback Skills into
// skillsDir. Extracted so codex_init_test.go can drive it directly.
func installCodexSkills(skillsDir string) error {
	desc1, body1, err := parseSlashCommandSource(claudeSlashCommandTemplate)
	if err != nil {
		return fmt.Errorf("crush skill: %w", err)
	}
	content1 := toSkillMD("crush", desc1, body1)
	if err := writeSentinelledSkillDir(skillsDir, "crush", claudeSlashCommandSentinel, content1); err != nil {
		return fmt.Errorf("crush skill: %w", err)
	}

	desc2, body2, err := parseSlashCommandSource(claudeFallbackCommandTemplate)
	if err != nil {
		return fmt.Errorf("crush-fallback skill: %w", err)
	}
	content2 := toSkillMD("crush-fallback", desc2, body2)
	if err := writeSentinelledSkillDir(skillsDir, "crush-fallback", claudeSlashCommandSentinel, content2); err != nil {
		return fmt.Errorf("crush-fallback skill: %w", err)
	}
	return nil
}

func init() {
	codexInitCmd.Flags().Bool("global", false, "Install into ~/.agents/skills/ (available in every project). Default when neither --global nor --local is given.")
	codexInitCmd.Flags().Bool("local", false, "Install into the current project's .agents/skills/ instead of ~/.agents/skills/.")
	rootCmd.AddCommand(codexInitCmd)
}
