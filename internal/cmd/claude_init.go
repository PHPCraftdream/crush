// Fork patch: batch 25 — rename model slash-commands from `<model>-<digit>`
// (o47-0..o47-4) to letter-suffix notation (o47l..o47xx) and add top-model
// shortcuts (ol, om, oh, ox, oxx, sl, sm, sh, sx, hl, hm, hh).
//
// Also in batch 25: `claude-init` deletes old-format files that carry our sentinel.
//
// Fork patch: batch 23 — `claude-init` also installs per-model slash commands.
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
	claudeModelAgentSentinel   = "<!-- crush-model-agent:v1 -->"
	claudeCommandsDir          = ".claude/commands"
	claudeGlobalCommandsDir    = ".claude/commands" // relative to $HOME
	claudeAgentsDir            = ".claude/agents"
	claudeGlobalAgentsDir      = ".claude/agents" // relative to $HOME
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

// resolveAgentsDir returns the directory where sub-agents should be written.
// When global is true it returns ~/.claude/agents; otherwise it returns
// <cwd>/.claude/agents.
func resolveAgentsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, claudeGlobalAgentsDir), nil
	}
	return filepath.Join(cwd, claudeAgentsDir), nil
}

// modelCmd describes one per-model slash command.
type modelCmd struct {
	name    string // filename without .md, e.g. "o47-3"
	model   string // full Anthropic model ID
	effort  string // low/medium/high/xhigh/max
	display string // human-readable label for description
}

// allModelCommands is the canonical list of per-model slash commands.
var allModelCommands = []modelCmd{
	// Opus 4.7 — low medium high xhigh max
	{"o47l", "claude-opus-4-7", "low", "Opus 4.7 (1M) – low"},
	{"o47m", "claude-opus-4-7", "medium", "Opus 4.7 (1M) – medium"},
	{"o47h", "claude-opus-4-7", "high", "Opus 4.7 (1M) – high"},
	{"o47x", "claude-opus-4-7", "xhigh", "Opus 4.7 (1M) – xhigh"},
	{"o47xx", "claude-opus-4-7", "max", "Opus 4.7 (1M) – max"},
	// Opus 4.6 — low medium high max (no xhigh)
	{"o46l", "claude-opus-4-6", "low", "Opus 4.6 (1M) – low"},
	{"o46m", "claude-opus-4-6", "medium", "Opus 4.6 (1M) – medium"},
	{"o46h", "claude-opus-4-6", "high", "Opus 4.6 (1M) – high"},
	{"o46xx", "claude-opus-4-6", "max", "Opus 4.6 (1M) – max"},
	// Sonnet 4.6 — low medium high max (no xhigh)
	{"s46l", "claude-sonnet-4-6", "low", "Sonnet 4.6 (200k) – low"},
	{"s46m", "claude-sonnet-4-6", "medium", "Sonnet 4.6 (200k) – medium"},
	{"s46h", "claude-sonnet-4-6", "high", "Sonnet 4.6 (200k) – high"},
	{"s46xx", "claude-sonnet-4-6", "max", "Sonnet 4.6 (200k) – max"},
	// Sonnet 4.5 — low medium high
	{"s45l", "claude-sonnet-4-5", "low", "Sonnet 4.5 (200k) – low"},
	{"s45m", "claude-sonnet-4-5", "medium", "Sonnet 4.5 (200k) – medium"},
	{"s45h", "claude-sonnet-4-5", "high", "Sonnet 4.5 (200k) – high"},
	// Haiku 4.5 — low medium high
	{"h45l", "claude-haiku-4-5", "low", "Haiku 4.5 (200k) – low"},
	{"h45m", "claude-haiku-4-5", "medium", "Haiku 4.5 (200k) – medium"},
	{"h45h", "claude-haiku-4-5", "high", "Haiku 4.5 (200k) – high"},
	// Top-model shortcuts (point to top version of each family)
	{"ol", "claude-opus-4-7", "low", "Opus (top, 1M) – low"},
	{"om", "claude-opus-4-7", "medium", "Opus (top, 1M) – medium"},
	{"oh", "claude-opus-4-7", "high", "Opus (top, 1M) – high"},
	{"ox", "claude-opus-4-7", "xhigh", "Opus (top, 1M) – xhigh"},
	{"oxx", "claude-opus-4-7", "max", "Opus (top, 1M) – max"},
	{"sl", "claude-sonnet-4-6", "low", "Sonnet (top, 200k) – low"},
	{"sm", "claude-sonnet-4-6", "medium", "Sonnet (top, 200k) – medium"},
	{"sh", "claude-sonnet-4-6", "high", "Sonnet (top, 200k) – high"},
	{"sx", "claude-sonnet-4-6", "max", "Sonnet (top, 200k) – max"},
	{"hl", "claude-haiku-4-5", "low", "Haiku (top, 200k) – low"},
	{"hm", "claude-haiku-4-5", "medium", "Haiku (top, 200k) – medium"},
	{"hh", "claude-haiku-4-5", "high", "Haiku (top, 200k) – high"},
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

  2. Write 31 per-model slash commands (versioned + top-model shortcuts):

       o47l..o47xx  claude-opus-4-7    1M ctx   effort low→max
       o46l..o46xx  claude-opus-4-6    1M ctx   effort low→max (no xhigh)
       s46l..s46xx  claude-sonnet-4-6  200k ctx effort low→max (no xhigh)
       s45l..s45h   claude-sonnet-4-5  200k ctx effort low→high
       h45l..h45h   claude-haiku-4-5   200k ctx effort low→high
       ol,om,oh,ox,oxx  claude-opus-4-7    1M ctx   (top opus shortcuts)
       sl,sm,sh,sx      claude-sonnet-4-6  200k ctx (top sonnet shortcuts)
       hl,hm,hh         claude-haiku-4-5   200k ctx (top haiku shortcuts)

     Each passes ` + "`$ARGUMENTS`" + ` straight to the chosen model/effort.
     Existing files without our sentinel are left alone.

  3. Write 31 per-model sub-agents (a<short-code>.md):
       ao47l..ao47xx, ao46l..ao46xx, as46l..as46xx, as45l..as45h,
       ah45l..ah45h, aol..aoxx, asl..asx, ahl..ahh

     Each mirrors its slash-command twin but runs in an ISOLATED context
     (fresh 1M/200k window, returns only a summary to the parent chat).

  4. Delete any old-format files (o47-0.md..h45-2.md) that carry our sentinel.

  5. Strip any pre-existing crush-claude-init block from ` + "`CLAUDE.md`" + `
     (any version v1..vN). If the file becomes empty it is removed.

` + "`claude-init`" + ` no longer writes anything into ` + "`CLAUDE.md`" + `. Delegation is
explicit-only — invoke ` + "`/crush <task>`" + ` or ` + "`/o47x <task>`" + ` when you want it.`,
	Example: `
# Install / refresh all slash-commands in the current workspace (local)
crush claude-init

# Install globally for every project (~/.claude/commands/)
crush claude-init --global

# Scope to another project
crush claude-init --cwd /path/to/project

# After init, in Claude Code you can type:
#   /o47x explain this function          → Opus 4.7 xhigh, same conversation
#   /s46m fix the lint warnings          → Sonnet 4.6 medium, same conversation
#   /h45l summarise this file            → Haiku 4.5 low, same conversation
#   /oh   deep analysis                  → Opus (top) high, same conversation

# Slash-commands continue current conversation; sub-agents run in fresh context:
#   /ao47x analyze codebase, return list → Opus 4.7 xhigh, isolated, returns summary only
#   /as46m refactor this module          → Sonnet 4.6 medium, isolated, returns summary only
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")

		var (
			cmdDir    string
			agentsDir string
			cwd       string
		)
		if global {
			if cmd.Flags().Changed("cwd") {
				return fmt.Errorf("--global and --cwd are mutually exclusive")
			}
			var err error
			cmdDir, err = resolveCommandsDir("", true)
			if err != nil {
				return err
			}
			agentsDir, err = resolveAgentsDir("", true)
			if err != nil {
				return err
			}
		} else {
			var err error
			cwd, err = ResolveCwd(cmd)
			if err != nil {
				return err
			}
			// 1. Strip any legacy crush-claude-init block from CLAUDE.md (local only).
			if err := stripLegacyBlockFromCLAUDEMd(cwd); err != nil {
				return err
			}
			cmdDir = filepath.Join(cwd, claudeCommandsDir)
			agentsDir = filepath.Join(cwd, claudeAgentsDir)
		}

		// 2. Install / refresh the /crush slash-command.
		if err := writeSlashCommandToDir(cmdDir); err != nil {
			return fmt.Errorf("slash command: %w", err)
		}

		// 3. Install / refresh per-model slash commands.
		if err := writeModelCommandsToDir(cmdDir); err != nil {
			return fmt.Errorf("model commands: %w", err)
		}

		// 4. Install / refresh per-model sub-agents.
		if err := writeModelAgentsToDir(agentsDir); err != nil {
			return fmt.Errorf("model agents: %w", err)
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

## Launching

Defaults to apply unless the user said otherwise:

- ` + "`--role smart`" + ` for non-trivial work; ` + "`--role fast`" + ` for one-liners.
- A stable, task-meaningful ` + "`--session`" + ` id (issue / branch / topic slug).
  Same id continues across runs.
- ` + "`--timeout`" + ` proportional to the scope. Rough rule of thumb:
  one-line tweak / single small file → ` + "`--timeout 5m`" + `; new file
  under ~300 lines → ` + "`10m`" + `; refactor across 2–4 files or any
  file over ~500 lines → ` + "`20m`" + `; deep bug-hunt or multi-package
  → ` + "`30m`" + `. When in doubt, over-provision — a 30m timeout costs
  nothing if the task finishes in 3m, but a 5m timeout that fires mid-edit
  leaves you with partial state.
- Launch in the background (` + "`Bash`" + ` with ` + "`run_in_background: true`" + `),
  redirect ` + "`> .crush/stdin/<task>.out 2>.crush/stdin/<task>.err`" + `, and react
  when the harness fires the completion notification. Do NOT poll with sleep.
  (Yes, the folder is called ` + "`stdin/`" + ` even though it also holds
  ` + "`.out`" + ` and ` + "`.err`" + ` outputs — it's a single per-task working
  directory. Don't let the name confuse you.)
- For multi-line prompts, ` + "`Write`" + ` them to a file under
  ` + "`./.crush/stdin/<task-slug>.prompt`" + ` and feed via stdin (` + "`< file`" + `).
  Avoid positional ` + "`\"…\"`" + ` for anything past one line.
- Permissions inside ` + "`crush run`" + ` are auto-approved (no human at the keyboard).
  Run only in workspaces you can afford to lose.
- **Parallel runs**: when fan-out is more than one ` + "`crush run`" + `, every
  prompt MUST explicitly name the file-set it is allowed to touch
  (e.g. "only edit ` + "`internal/foo/`" + ` and ` + "`docs/foo.md`" + `; do
  not touch root configs"). Two concurrent runs writing the same file
  race each other's edits and produce silent corruption.

## Monitoring a running session

Check whether a session is still alive via its lock heartbeat:

` + "```" + `
crush sessions locks
` + "```" + `

PULSE column meaning (heartbeat every 10 s, stale after 20 s):
- ` + "`alive`" + `    — last heartbeat ≤ 10 s ago, agent is running
- ` + "`ping`" + `     — 10–15 s ago, likely still running
- ` + "`stopping`" + ` — 15–20 s ago, agent is finishing or slow
- ` + "`offline`" + `  — >20 s ago, lock is stale (agent crashed or exited)

Show the last messages of a session to see what it produced:

` + "```" + `
crush sessions last <session-id>          # last 10 messages
crush sessions last <session-id> --n 3   # last 3 messages
` + "```" + `

Live-follow a running session:

` + "```" + `
crush sessions tail <session-id> --follow
` + "```" + `

List all sessions:

` + "```" + `
crush sessions list
crush sessions show <session-id> --with-messages
` + "```" + `

## Repo-wide default system prompt

If ` + "`./.crush/system-prompts/default.md`" + ` exists, ` + "`crush run`" + `
auto-loads it as the system prompt when neither ` + "`--system-prompt`" + `
nor ` + "`--system-prompt-file`" + ` was passed. Use this to commit one set
of "always apply" rules to the repo (scope-control, summary-required,
no-commits) instead of repeating them in every prompt.

Recommended starter template — write to ` + "`./.crush/system-prompts/default.md`" + `:

` + "```markdown" + `
You are a sub-agent invoked by an orchestrator. Apply these rules to
every task in this repo, in addition to anything in the user prompt:

1. **Stay strictly in scope.** Edit ONLY the files the prompt names.
   Do not refactor unrelated code, generalise patterns, expand
   .gitignore beyond what's asked, or "tidy up" while you're nearby.
   If you notice unrelated mess, list it in your final summary and
   leave it untouched.
2. **End every turn with a final assistant message** that names:
   files you changed, tests you ran, and any noteworthy observation.
   Wrappers parse the final_text — leaving it empty silently is a bug.
3. **Never commit, never push** unless the prompt explicitly says so.
4. **Run the tests** that cover what you touched before declaring done.
5. If you hit an ambiguity that needs a real product decision, stop
   and surface it — don't guess and ship.
` + "```" + `

Explicit ` + "`--system-prompt-file`" + ` always wins over the auto-loaded
default.

## When the lock is stuck

If a session reports "session is already in use" but you know the holder
is dead (TaskStop killed only the shell wrapper, not the underlying crush
process; the box rebooted; previous run was force-killed), do not try to
` + "`rm`" + ` the lock file manually — on Windows the OS still considers
it open and refuses. Use:

` + "```" + `
crush sessions kill <id>            # kills the holder PID + removes the lock
crush sessions reset <id> --force   # same, then also wipes message history
` + "```" + `

After either, ` + "`crush run --session <id>`" + ` can re-enter cleanly.

## After the run finishes

1. ` + "`Read`" + ` the result file (` + "`.crush/stdin/<task>.out`" + `).
   With ` + "`--json`" + ` it is the wire envelope; with default mode it
   is the model's final text.
2. **Always sanity-check with ` + "`git status --short`" + `** — the
   envelope's ` + "`final_text`" + ` is what the MODEL claims it did,
   not what it actually wrote to disk. Models occasionally edit files
   outside the asked scope (e.g. "tidying up" ` + "`.gitignore`" + ` when
   you only asked for one new line). If ` + "`git status`" + ` shows
   files outside the task's declared scope, ` + "`git checkout HEAD --`" + `
   them and re-prompt with tighter constraints.
3. Check ` + "`.warnings[]`" + ` in the JSON envelope. Specifically:
   ` + "`final_text is empty`" + ` means the model ended on a tool_call
   without composing a reply — fall back to ` + "`git status`" + ` plus
   ` + "`crush sessions last <id>`" + ` for context.
4. Apply any small tactical fixes yourself (typos, missed imports);
   re-delegate to the same ` + "`--session`" + ` for anything bigger.
5. Report back to the user with the summary + cost + what changed.

(` + "`crush sessions last <id>`" + ` is only needed when the ` + "`.out`" + `
file is missing or you are doing post-mortem audit of an old session.
For the just-finished run, the ` + "`.out`" + ` file already has the
envelope — read that.)

## Task

$ARGUMENTS
`
}

func writeModelCommands(cwd string) error {
	return writeModelCommandsToDir(filepath.Join(cwd, claudeCommandsDir))
}

// oldFormatNames lists old-style command file bases (o47-0..h45-2) to clean up.
var oldFormatNames = func() []string {
	var names []string
	for _, pfx := range []string{"o47", "o46", "s46", "s45", "h45"} {
		max := 4
		if pfx == "s45" || pfx == "h45" {
			max = 2
		} else if pfx == "o46" || pfx == "s46" {
			max = 3
		}
		for i := 0; i <= max; i++ {
			names = append(names, fmt.Sprintf("%s-%d", pfx, i))
		}
	}
	return names
}()

// removeOldFormatModelCommands deletes old-style command files that carry our sentinel.
func removeOldFormatModelCommands(dir string) {
	for _, name := range oldFormatNames {
		path := filepath.Join(dir, name+".md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), claudeModelCmdSentinel) {
			_ = os.Remove(path)
		}
	}
}

// writeModelCommandsToDir installs one <name>.md per entry in allModelCommands
// into dir. Files we don't own (missing sentinel) are left alone with a warning.
// Also removes any old-format files (o47-0..h45-2) that carry our sentinel.
func writeModelCommandsToDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Remove old-format files left over from previous installs.
	removeOldFormatModelCommands(dir)

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
		content := "---\n" +
			"description: " + claudeModelCmdSentinel + " " + mc.model + " effort=" + mc.effort + "\n" +
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

// buildAgentContent returns the body of `.claude/agents/a<name>.md`.
func buildAgentContent(mc modelCmd) string {
	return "---\n" +
		"name: a" + mc.name + "\n" +
		"description: " + claudeModelAgentSentinel + " " + mc.model + " effort=" + mc.effort + " (" + mc.display + ") — delegate task in isolated context\n" +
		"model: " + mc.model + "\n" +
		"---\n\n" +
		"You are a delegated worker invoked with reasoning effort: " + mc.effort + ".\n\n" +
		"The user passed:\n\n" +
		"$ARGUMENTS\n\n" +
		"Do the task autonomously. Return only the final result — no preamble, no recap of steps. If the task is a question, answer it directly. If it's an action, do it and report what changed.\n"
}

// writeModelAgentsToDir installs one a<name>.md per entry in allModelCommands
// into dir. Files we don't own (missing sentinel) are left alone with a warning.
func writeModelAgentsToDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	for _, mc := range allModelCommands {
		path := filepath.Join(dir, "a"+mc.name+".md")
		if data, err := os.ReadFile(path); err == nil {
			if !strings.Contains(string(data), claudeModelAgentSentinel) {
				fmt.Fprintf(os.Stderr, "warning: %s exists but is not ours — skipping\n", path)
				continue
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := os.WriteFile(path, []byte(buildAgentContent(mc)), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	fmt.Fprintf(os.Stderr, "wrote %d model agents to %s\n", len(allModelCommands), dir)
	return nil
}

func init() {
	claudeInitCmd.Flags().Bool("global", false, "Install into ~/.claude/commands/ (available in every project)")
	rootCmd.AddCommand(claudeInitCmd)
}
