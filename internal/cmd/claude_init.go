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
// Sentinel marker is prepended so claude-del can recognise files we own
// without parsing content. Self-contained: holds the full delegation
// guidance inline, because there is no longer a long block in CLAUDE.md
// to refer the operator to. Triggered ONLY by an explicit `/crush <task>`
// from the operator.
func claudeSlashCommandContent() string {
	return claudeSlashCommandSentinel + "\n" + claudeSlashCommandTemplate
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
