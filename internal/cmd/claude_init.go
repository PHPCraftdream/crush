package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// claudeInitMarkerStart is searched for to decide whether the snippet
// already exists. Bumping the v<N> version forces a re-write on the
// next run (old block is rewritten, not duplicated).
const (
	claudeInitMarkerStart = "<!-- crush-claude-init:v1 -->"
	claudeInitMarkerEnd   = "<!-- /crush-claude-init -->"
	claudeMdFile          = "CLAUDE.md"
)

var claudeInitCmd = &cobra.Command{
	Use:   "claude-init",
	Short: "Append a 'how to delegate work to crush' block to CLAUDE.md",
	Long: `Append a block of instructions to the workspace's CLAUDE.md that
teaches a Claude Code (or any other LLM following CLAUDE.md) how to
delegate work to ` + "`crush run`" + `: when to use the fast vs smart role,
how to pick stable session ids, how to parse --json output, and which
read-only commands are safe to discover state.

Idempotent: the inserted block is wrapped in a versioned sentinel
("` + claudeInitMarkerStart + `"). If the marker is already present, the
command exits without touching the file. If you want to refresh the
block, delete the existing one (or bump the version on disk) and re-run.

If CLAUDE.md does not exist yet it is created with the block as its
only content.`,
	Example: `
# Append the block in the current workspace
crush claude-init

# Or scope it to another project
crush claude-init --cwd /path/to/project
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		path := filepath.Join(cwd, claudeMdFile)

		var existing []byte
		if data, readErr := os.ReadFile(path); readErr == nil {
			existing = data
		} else if !os.IsNotExist(readErr) {
			return fmt.Errorf("failed to read %s: %w", path, readErr)
		}

		if !force && strings.Contains(string(existing), claudeInitMarkerStart) {
			fmt.Fprintf(os.Stderr, "%s already contains %s — nothing to do (use --force to re-append)\n", path, claudeInitMarkerStart)
			return nil
		}

		out := strings.Builder{}
		if len(existing) > 0 {
			out.Write(existing)
			// Ensure a blank line between prior content and our block.
			if !strings.HasSuffix(string(existing), "\n\n") {
				if !strings.HasSuffix(string(existing), "\n") {
					out.WriteString("\n")
				}
				out.WriteString("\n")
			}
		}
		out.WriteString(claudeInitBlock())

		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		action := "appended to"
		if len(existing) == 0 {
			action = "created"
		}
		fmt.Fprintf(os.Stderr, "%s %s\n", action, path)
		return nil
	},
}

func init() {
	claudeInitCmd.Flags().Bool("force", false, "Re-append the block even if the marker is already present")
	rootCmd.AddCommand(claudeInitCmd)
}

func claudeInitBlock() string {
	return claudeInitMarkerStart + `
## Delegate heavy work to ` + "`crush`" + `

This workspace has [crush](https://github.com/charmbracelet/crush) installed.
` + "`crush`" + ` is a CLI agent with its own persistent sessions, its own LLM provider
config, and its own approval policy. Use it as a **sub-agent** when running
the work yourself would burn through your context, when several tasks can
proceed in parallel, or when a task is exploratory enough that you'd
rather not pay for the false starts in your own scrollback.

### When to delegate vs do it yourself

Delegate to ` + "`crush`" + ` when **any** of these are true:

- the task touches more files than you can hold in your head at once
  (large refactors, repo-wide renames, codebase exploration);
- the task is repetitive (apply pattern X to every file matching Y);
- the task is open-ended exploration likely to spawn a lot of tool
  calls before producing the answer you actually want;
- you want several attempts in parallel ("try approach A, B, and C and
  tell me which one passes the tests");
- the user is fine with you working in the background while they keep
  the conversation going.

Do it yourself when the task is short, depends on context from the
current conversation that's hard to serialise, or when fast feedback to
the user matters more than offloading the work.

### Quick patterns

**One-shot with the cheap model, machine-readable result**:
` + "```bash" + `
crush run --role fast --json \
  "summarise the last 200 lines of dev.log" < dev.log
` + "```" + `

**Long task with a stable session id (continues across invocations)**:
` + "```bash" + `
crush run --role smart --session "refactor-storage" \
  --system-prompt-file ./prompts/refactor.md \
  "refactor internal/storage to use the new interface"
` + "```" + `

**Bounded by a deadline; structured result for parsing**:
` + "```bash" + `
crush run --role smart --timeout 10m --session "deploy-check" --json \
  "verify the deploy is green; if not, summarise what failed"
` + "```" + `

**Capture only the final text** (heartbeat goes to stderr, drop it):
` + "```bash" + `
crush run --role fast --json "..." 2>/dev/null | jq -r .final_text
` + "```" + `

### Conventions

- ` + "`--role`" + ` is **required**. ` + "`smart`" + ` (or ` + "`large`" + `) for the strong/slow
  model, ` + "`fast`" + ` (or ` + "`small`" + `) for the cheap/quick one. Default would have
  silently burned premium tokens, so it has to be declared.
- ` + "`--json`" + ` whenever you'll parse the result — final text, exit reason,
  per-tool call counts, token usage, duration are all on one object.
- ` + "`--session <id>`" + ` is get-or-create: pick a stable, task-meaningful id
  (issue number, branch name, feature slug). Same id continues the same
  conversation; new id starts a fresh one.
- ` + "`--system-prompt-file <path>`" + ` to lock the agent into a specific role
  (reviewer, test-writer, refactorer). The prompt persists on the session
  so follow-up runs inherit it automatically.
- Permissions are **auto-approved** inside ` + "`crush run`" + ` — no human is on
  the keyboard to confirm. Run only in workspaces you can afford to lose,
  and prefer ` + "`--cwd /tmp/sandbox`" + ` or a worktree for risky calls.

### Read-only discovery commands (always safe)

- ` + "`crush providers list`" + ` — which providers are configured and which
  have credentials.
- ` + "`crush models show`" + ` — which model fills the smart and fast slots.
- ` + "`crush sessions list`" + ` — past conversations, with token cost.
- ` + "`crush system-prompt --session <id>`" + ` — exact prompt the next turn
  would send. Round-trip into a file, edit it, write back with
  ` + "`crush run --system-prompt-file ...`" + `.

### Lifecycle housekeeping

After a task ends and you don't need the context anymore:

` + "```bash" + `
crush sessions delete "<id>"     # remove session + messages
# or to retry with the same id and the same configured system prompt:
crush sessions reset  "<id>"     # wipe messages, keep id + role
` + "```" + `

### Background-friendly

Launch ` + "`crush run ...`" + ` in the background, keep talking to the user, and
pick up the result when the process exits — the run is fully detached
from your shell.
` + claudeInitMarkerEnd + `
`
}
