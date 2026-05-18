// Fork patch: batch 23 — `claude-init` also installs per-model slash commands
// (o47-0..o47-4, o46-0..o46-3, s46-0..s46-3, s45-0..s45-2, h45-0..h45-2).
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
	claudeModelCmdSentinel     = "<!-- crush-model-command:v1 -->"
	claudeCommandsDir          = ".claude/commands"
)

// modelCmd describes one per-model slash command.
type modelCmd struct {
	name    string // filename without .md, e.g. "o47-3"
	model   string // full Anthropic model ID
	effort  string // low/medium/high/xhigh/max
	display string // human-readable label for description
}

// allModelCommands is the canonical list of per-model slash commands.
var allModelCommands = []modelCmd{
	// Opus 4.7 — low(0) medium(1) high(2) xhigh(3) max(4)
	{"o47-0", "claude-opus-4-7", "low", "Opus 4.7 – low"},
	{"o47-1", "claude-opus-4-7", "medium", "Opus 4.7 – medium"},
	{"o47-2", "claude-opus-4-7", "high", "Opus 4.7 – high"},
	{"o47-3", "claude-opus-4-7", "xhigh", "Opus 4.7 – xhigh"},
	{"o47-4", "claude-opus-4-7", "max", "Opus 4.7 – max"},
	// Opus 4.6 — low(0) medium(1) high(2) max(3)
	{"o46-0", "claude-opus-4-6", "low", "Opus 4.6 – low"},
	{"o46-1", "claude-opus-4-6", "medium", "Opus 4.6 – medium"},
	{"o46-2", "claude-opus-4-6", "high", "Opus 4.6 – high"},
	{"o46-3", "claude-opus-4-6", "max", "Opus 4.6 – max"},
	// Sonnet 4.6 — low(0) medium(1) high(2) max(3)
	{"s46-0", "claude-sonnet-4-6", "low", "Sonnet 4.6 – low"},
	{"s46-1", "claude-sonnet-4-6", "medium", "Sonnet 4.6 – medium"},
	{"s46-2", "claude-sonnet-4-6", "high", "Sonnet 4.6 – high"},
	{"s46-3", "claude-sonnet-4-6", "max", "Sonnet 4.6 – max"},
	// Sonnet 4.5 — low(0) medium(1) high(2)
	{"s45-0", "claude-sonnet-4-5", "low", "Sonnet 4.5 – low"},
	{"s45-1", "claude-sonnet-4-5", "medium", "Sonnet 4.5 – medium"},
	{"s45-2", "claude-sonnet-4-5", "high", "Sonnet 4.5 – high"},
	// Haiku 4.5 — low(0) medium(1) high(2)
	{"h45-0", "claude-haiku-4-5", "low", "Haiku 4.5 – low"},
	{"h45-1", "claude-haiku-4-5", "medium", "Haiku 4.5 – medium"},
	{"h45-2", "claude-haiku-4-5", "high", "Haiku 4.5 – high"},
}

var claudeInitCmd = &cobra.Command{
	Use:   "claude-init",
	Short: "Install /crush and per-model slash-commands locally; strip legacy CLAUDE.md block",
	Long: `Set up the current workspace (project-local) so an operator can delegate
tasks to crush or invoke a specific model directly from Claude Code.

All files are written to ` + "`.claude/commands/`" + ` inside the project directory
(or --cwd). This is the LOCAL scope — Claude Code also supports a global
scope at ` + "`~/.claude/commands/`" + `, which this command does NOT touch.

Concretely:

  1. Write ` + "`.claude/commands/crush.md`" + ` — the ` + "`/crush`" + ` delegation command.
     Skipped (with a warning) if the file exists without our sentinel.

  2. Write 19 per-model slash commands (` + "`/o47-0`" + `..` + "`/h45-2`" + `):

       o47-0..o47-4  claude-opus-4-7    effort low→max  (0=low 4=max)
       o46-0..o46-3  claude-opus-4-6    effort low→max
       s46-0..s46-3  claude-sonnet-4-6  effort low→max
       s45-0..s45-2  claude-sonnet-4-5  effort low→high
       h45-0..h45-2  claude-haiku-4-5   effort low→high

     Each passes ` + "`$ARGUMENTS`" + ` straight to the chosen model/effort.
     Existing files without our sentinel are left alone.

  3. Strip any pre-existing crush-claude-init block from ` + "`CLAUDE.md`" + `
     (any version v1..vN). If the file becomes empty it is removed.

` + "`claude-init`" + ` no longer writes anything into ` + "`CLAUDE.md`" + `. Delegation is
explicit-only — invoke ` + "`/crush <task>`" + ` or ` + "`/o47-3 <task>`" + ` when you want it.`,
	Example: `
# Install / refresh all slash-commands in the current workspace (local)
crush claude-init

# Scope to another project
crush claude-init --cwd /path/to/project

# After init, in Claude Code you can type:
#   /o47-3 explain this function   → Opus 4.7 xhigh
#   /s46-1 fix the lint warnings   → Sonnet 4.6 medium
#   /h45-0 summarise this file     → Haiku 4.5 low
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

		// 2. Install / refresh the /crush slash-command.
		if err := writeSlashCommand(cwd); err != nil {
			return fmt.Errorf("slash command: %w", err)
		}

		// 3. Install / refresh per-model slash commands.
		if err := writeModelCommands(cwd); err != nil {
			return fmt.Errorf("model commands: %w", err)
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

// writeModelCommands installs one .claude/commands/<name>.md per entry in
// allModelCommands. Each file contains a frontmatter model+effort directive
// and passes $ARGUMENTS straight through. Files we don't own (missing
// sentinel) are left alone with a warning.
func writeModelCommands(cwd string) error {
	dir := filepath.Join(cwd, claudeCommandsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for _, mc := range allModelCommands {
		path := filepath.Join(dir, mc.name+".md")
		if data, err := os.ReadFile(path); err == nil {
			if !strings.Contains(string(data), claudeModelCmdSentinel) {
				fmt.Fprintf(os.Stderr, "warning: %s exists but is not ours — skipping\n", path)
				continue
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		content := claudeModelCmdSentinel + "\n" +
			"---\n" +
			"description: " + mc.display + "\n" +
			"model: " + mc.model + "\n" +
			"effort: " + mc.effort + "\n" +
			"---\n\n" +
			"$ARGUMENTS\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	fmt.Fprintf(os.Stderr, "wrote %d model commands to %s\n", len(allModelCommands), dir)
	return nil
}

func init() {
	rootCmd.AddCommand(claudeInitCmd)
}
