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
	Short: "Remove the crush delegation block from CLAUDE.md",
	Long: `Undo ` + "`crush claude-init`" + `: strip every crush-claude-init
block (any version) from CLAUDE.md and remove the
.claude/commands/crush.md slash command (if it carries our sentinel).

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove from the current workspace
crush claude-del

# Scope to another project
crush claude-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		return runClaudeDel(cwd)
	},
}

func runClaudeDel(cwd string) error {
	// 1. Strip blocks from CLAUDE.md.
	claudeMdPath := filepath.Join(cwd, claudeMdFile)
	removed, err := stripClaudeMdBlocks(claudeMdPath)
	if err != nil {
		return err
	}

	// 2. Remove /crush slash command if ours.
	if err := removeSlashCommand(cwd); err != nil {
		return err
	}

	// 3. Remove per-model slash commands if ours.
	if err := removeModelCommands(cwd); err != nil {
		return err
	}

	_ = removed
	return nil
}

func removeModelCommands(cwd string) error {
	dir := filepath.Join(cwd, claudeCommandsDir)
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
		fmt.Fprintf(os.Stderr, "removed %d model commands\n", removed)
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

	// Write back with a single trailing newline.
	if err := os.WriteFile(path, []byte(cleaned+"\n"), 0o644); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "stripped %d crush-claude-init block(s) from %s\n", len(matches), claudeMdFile)
	return len(matches), nil
}

func removeSlashCommand(cwd string) error {
	path := filepath.Join(cwd, claudeSlashCommandPath)
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

func init() {
	rootCmd.AddCommand(claudeDelCmd)
}
