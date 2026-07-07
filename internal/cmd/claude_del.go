package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var claudeDelCmd = &cobra.Command{
	Use:   "claude-del",
	Short: "Remove the /crush slash-command and strip legacy CLAUDE.md block",
	Long: `Undo ` + "`crush claude-init`" + `: remove the /crush slash-command and strip
any crush-claude-init block from CLAUDE.md.

Only files that carry our sentinel are removed — foreign files with the
same name are left alone with a warning.

Default is --global (~/.claude/commands/). Use --local (or --cwd, which
implies it) to target the current project's .claude/commands/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.

For per-model commands, agents and skills, use ` + "`cah uninstall`" + ` from the
cc-arch-hands repo.`,
	Example: `
# Remove globally (from ~/.claude/commands/) — the default
crush claude-del

# Remove from the current project instead
crush claude-del --local

# Scope to another project (implies --local)
crush claude-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var cmdDir string
		if localMode {
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			// Strip CLAUDE.md blocks only in local mode.
			claudeMdPath := filepath.Join(cwd, claudeMdFile)
			if _, err := stripClaudeMdBlocks(claudeMdPath); err != nil {
				return err
			}
			cmdDir = filepath.Join(cwd, claudeCommandsDir)
		} else {
			// Default (no flags), or explicit --global: global mode.
			var err error
			cmdDir, err = resolveCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeSlashCommandFromDir(cmdDir)
	},
}

// runClaudeDel is kept for tests that call it directly (local mode only).
func runClaudeDel(cwd string) error {
	claudeMdPath := filepath.Join(cwd, claudeMdFile)
	if _, err := stripClaudeMdBlocks(claudeMdPath); err != nil {
		return err
	}
	cmdDir := filepath.Join(cwd, claudeCommandsDir)
	return removeSlashCommandFromDir(cmdDir)
}

func removeSlashCommand(cwd string) error {
	return removeSlashCommandFromDir(filepath.Join(cwd, claudeCommandsDir))
}

func removeSlashCommandFromDir(dir string) error {
	path := filepath.Join(dir, "crush.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.Contains(string(data), claudeSlashCommandSentinel) {
		fmt.Fprintf(os.Stderr, "refusing to delete %s — does not look like ours (missing sentinel)\n", path)
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", path)
	return nil
}

func stripClaudeMdBlocks(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no %s found — nothing to do\n", claudeMdFile)
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read %s: %w", path, err)
	}

	body := string(data)
	matches := claudeInitBlockPattern.FindAllString(body, -1)
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "no crush-claude-init block found in %s\n", path)
		return 0, nil
	}

	cleaned := claudeInitBlockPattern.ReplaceAllString(body, "")
	cleaned = strings.TrimRight(cleaned, " \t\n")

	if cleaned == "" {
		if err := os.Remove(path); err != nil {
			return 0, fmt.Errorf("failed to remove %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "removed empty %s\n", claudeMdFile)
		return len(matches), nil
	}

	if err := os.WriteFile(path, []byte(cleaned+"\n"), 0o644); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "stripped %d crush-claude-init block(s) from %s\n", len(matches), claudeMdFile)
	return len(matches), nil
}

func init() {
	claudeDelCmd.Flags().Bool("global", false, "Remove from ~/.claude/commands/. Default when neither --global nor --local is given.")
	claudeDelCmd.Flags().Bool("local", false, "Remove from the current project's .claude/commands/ instead of ~/.claude/commands/.")
	rootCmd.AddCommand(claudeDelCmd)
}
