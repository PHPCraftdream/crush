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
	Short: "Remove crush slash-commands and legacy CLAUDE.md block",
	Long: `Undo ` + "`crush claude-init`" + `: remove the /crush slash-command, all
per-model slash-commands (o47-*, s46-*, h45-*, …), and strip any
crush-claude-init block from CLAUDE.md.

Only files that carry our sentinel are removed — foreign files with the
same names are left alone with a warning.

Use --global to remove from ~/.claude/commands/ instead of the local
.claude/commands/ directory. --global and --cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove from the current workspace
crush claude-del

# Remove globally (from ~/.claude/commands/)
crush claude-del --global

# Scope to another project
crush claude-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")

		var cmdDir string
		if global {
			if cmd.Flags().Changed("cwd") {
				return fmt.Errorf("--global and --cwd are mutually exclusive")
			}
			var err error
			cmdDir, err = resolveCommandsDir("", true)
			if err != nil {
				return err
			}
		} else {
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
		}

		if err := removeSlashCommandFromDir(cmdDir); err != nil {
			return err
		}
		return removeModelCommandsFromDir(cmdDir)
	},
}

// runClaudeDel is kept for tests that call it directly (local mode only).
func runClaudeDel(cwd string) error {
	claudeMdPath := filepath.Join(cwd, claudeMdFile)
	if _, err := stripClaudeMdBlocks(claudeMdPath); err != nil {
		return err
	}
	dir := filepath.Join(cwd, claudeCommandsDir)
	if err := removeSlashCommandFromDir(dir); err != nil {
		return err
	}
	return removeModelCommandsFromDir(dir)
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

func removeModelCommands(cwd string) error {
	return removeModelCommandsFromDir(filepath.Join(cwd, claudeCommandsDir))
}

func removeModelCommandsFromDir(dir string) error {
	removed := 0
	for _, mc := range allModelCommands {
		path := filepath.Join(dir, mc.name+".md")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read %s: %w", path, err)
		}
		if !strings.Contains(string(data), claudeModelCmdSentinel) {
			fmt.Fprintf(os.Stderr, "refusing to delete %s — missing sentinel\n", path)
			continue
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		removed++
	}
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "removed %d model commands from %s\n", removed, dir)
	}
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
	claudeDelCmd.Flags().Bool("global", false, "Remove from ~/.claude/commands/ instead of the local .claude/commands/")
	rootCmd.AddCommand(claudeDelCmd)
}
