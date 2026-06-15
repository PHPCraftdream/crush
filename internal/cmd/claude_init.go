// Fork patch: `claude-init` installs ONLY the `/crush` slash-command and
// strips any legacy crush-claude-init block from CLAUDE.md. The per-model
// slash-commands, per-model sub-agents and model-registry / agent-clause /
// sentinel code have been extracted to a separate repo (`cc-arch-hands`);
// use `cah install` from there for the rest.
//
// On invocation we still STRIP any pre-existing crush-claude-init block
// from CLAUDE.md (any version) so users upgrading from an older crush
// get a clean workspace. If the strip leaves CLAUDE.md empty, the file
// is removed (mirrors `claude-del`'s behaviour).
package cmd

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// claudeSlashCommandTemplate is the canonical /crush slash-command body
// minus the sentinel marker (which is prepended at write time so
// `claude-del` can recognise files we own without depending on file
// content semantics). Kept in a sibling .md file rather than a Go raw
// string so future edits don't need backtick / dollar-sign escaping.
//
//go:embed claude_slash_command.md
var claudeSlashCommandTemplate string

// claudeInitBlockPattern matches any version of the legacy inserted block —
// `<!-- crush-claude-init:v1 --> … <!-- /crush-claude-init -->`.
// Kept around because `claude-init` still runs it on existing CLAUDE.md
// files to GC the block when an old install is upgraded.
var claudeInitBlockPattern = regexp.MustCompile(`(?s)<!-- crush-claude-init:v\d+ -->.*?<!-- /crush-claude-init -->\s*`)

const (
	claudeMdFile               = "CLAUDE.md"
	claudeSlashCommandPath     = ".claude/commands/crush.md"
	claudeSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
	claudeCommandsDir          = ".claude/commands"
	claudeGlobalCommandsDir    = ".claude/commands" // relative to $HOME
)

// resolveCommandsDir returns the directory where slash commands should be
// written. When global is true it returns ~/.claude/commands; otherwise it
// returns <cwd>/.claude/commands.
func resolveCommandsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, claudeGlobalCommandsDir), nil
	}
	return filepath.Join(cwd, claudeCommandsDir), nil
}

var claudeInitCmd = &cobra.Command{
	Use:   "claude-init",
	Short: "Install the /crush slash-command and strip legacy CLAUDE.md block",
	Long: `Set up the current workspace (project-local) so an operator can delegate
tasks to crush from Claude Code via ` + "`/crush <task>`" + `.

The slash-command file is written to ` + "`.claude/commands/crush.md`" + ` inside the
project directory (or --cwd). This is the LOCAL scope — Claude Code also
supports a global scope at ` + "`~/.claude/commands/`" + ` (use --global).

Concretely:

  1. Write ` + "`.claude/commands/crush.md`" + ` — the ` + "`/crush`" + ` delegation command.
     Skipped (with a warning) if the file exists without our sentinel.

  2. Strip any pre-existing crush-claude-init block from ` + "`CLAUDE.md`" + `
     (any version v1..vN). If the file becomes empty it is removed.

` + "`claude-init`" + ` no longer writes anything into ` + "`CLAUDE.md`" + `. Delegation is
explicit-only — invoke ` + "`/crush <task>`" + ` when you want it.

For per-model commands, agents and skills, use ` + "`cah install`" + ` from the
cc-arch-hands repo.`,
	Example: `
# Install / refresh the /crush slash-command in the current workspace (local)
crush claude-init

# Install globally for every project (~/.claude/commands/)
crush claude-init --global

# Scope to another project
crush claude-init --cwd /path/to/project
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
			// Strip any legacy crush-claude-init block from CLAUDE.md (local only).
			if err := stripLegacyBlockFromCLAUDEMd(cwd); err != nil {
				return err
			}
			cmdDir = filepath.Join(cwd, claudeCommandsDir)
		}

		// Install / refresh the /crush slash-command.
		if err := writeSlashCommandToDir(cmdDir); err != nil {
			return fmt.Errorf("slash command: %w", err)
		}
		return nil
	},
}

// stripLegacyBlockFromCLAUDEMd removes every crush-claude-init block from
// the workspace's CLAUDE.md. If the file becomes empty, it is deleted.
// Missing file is a no-op.
func stripLegacyBlockFromCLAUDEMd(cwd string) error {
	path := filepath.Join(cwd, claudeMdFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	body := string(data)
	matches := claudeInitBlockPattern.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	stripped := strings.TrimRight(claudeInitBlockPattern.ReplaceAllString(body, ""), " \t\n")
	if stripped == "" {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("failed to remove now-empty %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "stripped %d legacy crush-claude-init block(s) and removed now-empty %s\n", len(matches), path)
		return nil
	}
	if err := os.WriteFile(path, []byte(stripped+"\n"), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "stripped %d legacy crush-claude-init block(s) from %s\n", len(matches), path)
	return nil
}

func writeSlashCommand(cwd string) error {
	return writeSlashCommandToDir(filepath.Join(cwd, claudeCommandsDir))
}

func writeSlashCommandToDir(dir string) error {
	path := filepath.Join(dir, "crush.md")
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), claudeSlashCommandSentinel) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(claudeSlashCommandContent()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// claudeSlashCommandContent returns the body of `.claude/commands/crush.md`.
// Sentinel marker is prepended so claude-del can recognise files we own
// without parsing content. Self-contained: holds the full delegation
// guidance inline, because there is no longer a long block in CLAUDE.md
// to refer the operator to. Triggered ONLY by an explicit `/crush <task>`
// from the operator.
func claudeSlashCommandContent() string {
	return claudeSlashCommandSentinel + "\n" + claudeSlashCommandTemplate
}

func init() {
	claudeInitCmd.Flags().Bool("global", false, "Install into ~/.claude/commands/ (available in every project)")
	rootCmd.AddCommand(claudeInitCmd)
}
