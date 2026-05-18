// Fork patch: batch 22 — `claude-init` no longer writes a delegation block
// into CLAUDE.md. The block (versions v1..v10) was the proximate cause of
// a recursive-delegation fork-bomb: any Claude Code session in the
// workspace read it on startup and tried to delegate every task back into
// `crush run`, which spawned another Claude Code session, which read the
// same block, and so on. agentguard (batch 16) + MCP-bridge re-activation
// (batch 20) close the cycle at the tool-call layer, but the cleanest
// fix is to remove the trigger entirely. `claude-init` now ONLY installs
// the `/crush` slash-command — that command is invoked explicitly by the
// operator when they actually want to delegate, never auto-discovered.
//
// On invocation we still STRIP any pre-existing crush-claude-init block
// from CLAUDE.md (any version) so users upgrading from an older crush
// get a clean workspace. If the strip leaves CLAUDE.md empty, the file
// is removed (mirrors `claude-del`'s behaviour).
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// claudeInitBlockPattern matches any version of the legacy inserted block —
// `<!-- crush-claude-init:v1 --> … <!-- /crush-claude-init -->`.
// Kept around because `claude-init` still runs it on existing CLAUDE.md
// files to GC the block when an old install is upgraded.
var claudeInitBlockPattern = regexp.MustCompile(`(?s)<!-- crush-claude-init:v\d+ -->.*?<!-- /crush-claude-init -->\s*`)

const (
	claudeMdFile               = "CLAUDE.md"
	claudeSlashCommandPath     = ".claude/commands/crush.md"
	claudeSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

var claudeInitCmd = &cobra.Command{
	Use:   "claude-init",
	Short: "Install the `/crush` slash-command and strip any legacy delegation block from CLAUDE.md",
	Long: `Set up the workspace so an operator can delegate a task to crush via the
` + "`/crush`" + ` slash command in Claude Code. Concretely:

  1. Write ` + "`.claude/commands/crush.md`" + ` (the slash-command body). If a file
     with that path already exists WITHOUT our sentinel, leave it alone and
     print a warning — someone else owns it.

  2. Strip any pre-existing crush-claude-init block from ` + "`CLAUDE.md`" + `
     (matches any version, v1..vN). This cleans up workspaces where an
     older crush installed an always-on delegation instruction; that block
     turned out to be a recursive-delegation footgun (see CHANGELOG.fork.md
     batch 22). If stripping leaves CLAUDE.md empty, the file is removed.

` + "`claude-init`" + ` no longer writes anything into ` + "`CLAUDE.md`" + `. Delegation is
explicit-only — the operator invokes ` + "`/crush <task>`" + ` when they want it.`,
	Example: `
# Install / refresh the slash-command in the current workspace
crush claude-init

# Scope to another project
crush claude-init --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}

		// 1. Strip any legacy crush-claude-init block from CLAUDE.md.
		if err := stripLegacyBlockFromCLAUDEMd(cwd); err != nil {
			return err
		}

		// 2. Install / refresh the slash-command.
		if err := writeSlashCommand(cwd); err != nil {
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
	path := filepath.Join(cwd, claudeSlashCommandPath)
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), claudeSlashCommandSentinel) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(claudeSlashCommandContent()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// claudeSlashCommandContent returns the body of `.claude/commands/crush.md`.
// Self-contained: holds the full delegation guidance inline, because there
// is no longer a long block in CLAUDE.md to refer the operator to.
// Triggered ONLY by an explicit `/crush <task>` from the operator.
func claudeSlashCommandContent() string {
	return claudeSlashCommandSentinel + `
---
description: Delegate this task to a crush sub-agent instead of doing it yourself
---

Do not implement the following task yourself. Build a ` + "`crush run`" + ` invocation
and launch it.

Defaults to apply unless the user said otherwise:

- ` + "`--role smart`" + ` for non-trivial work; ` + "`--role fast`" + ` for one-liners.
- A stable, task-meaningful ` + "`--session`" + ` id (issue / branch / topic slug).
  Same id continues across runs.
- ` + "`--timeout`" + ` proportional to the scope (5–15 min typical).
- Launch in the background (` + "`Bash`" + ` with ` + "`run_in_background: true`" + `),
  redirect ` + "`> .crush/stdin/<task>.out 2>.crush/stdin/<task>.err`" + `, and react
  when the harness fires the completion notification. Do NOT poll with sleep.
- For multi-line prompts, ` + "`Write`" + ` them to a file under
  ` + "`./.crush/stdin/<task-slug>.prompt`" + ` and feed via stdin (` + "`< file`" + `).
  Avoid positional ` + "`\"…\"`" + ` for anything past one line.
- Permissions inside ` + "`crush run`" + ` are auto-approved (no human at the keyboard).
  Run only in workspaces you can afford to lose.

Once the run finishes:

1. ` + "`Read`" + ` the result file.
2. Sanity-check the diff/output against the user's intent.
3. Apply any small tactical fixes yourself (typos, missed imports);
   re-delegate to the same ` + "`--session`" + ` for anything bigger.
4. Report back to the user with the summary + cost + what changed.

Task:

$ARGUMENTS
`
}

func init() {
	rootCmd.AddCommand(claudeInitCmd)
}
