package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// claudeInitBlockPattern matches any version of the inserted block —
// from the opening "crush-claude-init:vN" marker to the matching close
// marker, including a trailing newline. Multiline + lazy so two blocks
// in a row are excised separately, not merged into one giant span.
var claudeInitBlockPattern = regexp.MustCompile(`(?s)<!-- crush-claude-init:v\d+ -->.*?<!-- /crush-claude-init -->\s*`)

const (
	claudeInitMarkerStart      = "<!-- crush-claude-init:v10 -->"
	claudeInitMarkerEnd        = "<!-- /crush-claude-init -->"
	claudeMdFile               = "CLAUDE.md"
	claudeSlashCommandPath     = ".claude/commands/crush.md"
	claudeSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

var claudeInitCmd = &cobra.Command{
	Use:   "claude-init",
	Short: "Install or refresh the 'how to delegate work to crush' block in CLAUDE.md",
	Long: `Install (or refresh) a block of instructions in the workspace's
CLAUDE.md that teaches a Claude Code (or any other LLM following CLAUDE.md)
how to delegate work to ` + "`crush run`" + `: when to use the fast vs smart
role, how to pick stable session ids, how to parse --json output, and which
read-only commands are safe to discover state.

The block is wrapped in a versioned sentinel
("` + claudeInitMarkerStart + `"). Every invocation strips ALL previously
inserted blocks (any version) and writes a single fresh one — so re-running
after a fork update is always safe and idempotent.

If CLAUDE.md does not exist yet it is created with the block as its only
content.`,
	Example: `
# Install or refresh in the current workspace
crush claude-init

# Scope to another project
crush claude-init --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
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

		body := string(existing)
		matches := claudeInitBlockPattern.FindAllString(body, -1)
		removed := len(matches)
		body = claudeInitBlockPattern.ReplaceAllString(body, "")

		out := strings.Builder{}
		if len(body) > 0 {
			trimmed := strings.TrimRight(body, " \t\n")
			out.WriteString(trimmed)
			if trimmed != "" {
				out.WriteString("\n\n")
			}
		}
		out.WriteString(claudeInitBlock())

		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		switch {
		case removed > 0:
			fmt.Fprintf(os.Stderr, "replaced %d previous block(s) in %s\n", removed, path)
		case len(existing) == 0:
			fmt.Fprintf(os.Stderr, "created %s\n", path)
		default:
			fmt.Fprintf(os.Stderr, "appended to %s\n", path)
		}

		if err := writeSlashCommand(cwd); err != nil {
			return fmt.Errorf("slash command: %w", err)
		}
		return nil
	},
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
// Kept short on purpose: the heavy lifting (when to delegate, channels,
// patterns) lives in CLAUDE.md's claude-init block, which Claude Code
// reads automatically. This file is just the trigger that says "for THIS
// task, apply that block".
func claudeSlashCommandContent() string {
	return claudeSlashCommandSentinel + `
---
description: Delegate this task to a crush sub-agent instead of doing it yourself
---

Do not implement the following task yourself. Build a ` + "`crush run`" + ` invocation
using the rules in this repo's CLAUDE.md (search the file for
"crush-claude-init" — that block has the canonical patterns,
` + "`--role`" + ` / ` + "`--session`" + ` / ` + "`--json`" + ` semantics, and the DO-NOT-block-on-it
rules for background launches).

Defaults to apply unless the user said otherwise:

- ` + "`--role smart`" + ` for non-trivial work; ` + "`--role fast`" + ` for one-liners.
- A stable, task-meaningful ` + "`--session`" + ` id (issue / branch / topic slug).
  Same id continues across runs.
- ` + "`--timeout`" + ` proportional to the scope (5–15 min typical).
- Launch in the background (` + "`Bash`" + ` with ` + "`run_in_background: true`" + `),
  redirect ` + "`> /tmp/<task>.json 2>/tmp/<task>.err`" + `, and react when the
  harness fires the completion notification. Do NOT poll with sleep.
- For multi-line prompts, ` + "`Write`" + ` them to a file and feed via stdin
  (` + "`< file`" + `). Avoid positional ` + "`\"…\"`" + ` for anything past one line.

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

func claudeInitBlock() string {
	return claudeInitMarkerStart + `
## Working with ` + "`crush`" + `: you are the strategist, ` + "`crush`" + ` is the worker

This workspace has [crush](https://github.com/charmbracelet/crush)
installed. ` + "`crush`" + ` is a CLI agent with its own persistent sessions,
its own LLM provider config, and its own approval policy. Treat it as
your **execution arm**, not as a fallback you reach for when a task
"feels big".

### Default posture: delegate the doing, own the thinking

Your job here is to be the strategist, planner, reviewer, and the
person on the hook with the user. The actual *doing* — reading the
codebase, writing patches, running tests, debugging stack traces,
exploring unfamiliar dirs — belongs to ` + "`crush`" + ` sub-agents you launch.
This split exists for a concrete reason: your context window is the
scarce resource for *judgement* (the user's intent, the trade-offs,
the why), so spending it on raw implementation tokens is a waste.

What stays in your hand:

- **Understanding the user's intent** and re-interpreting fuzzy
  requests into concrete tasks.
- **Decomposing the work** into independently-shippable pieces with
  a clear pass criterion each. Writing the prompt for each piece is
  *your* high-leverage move.
- **Choosing the right role** (smart vs fast) and the right session
  topology (one shared session for iterative work, separate
  sessions for parallel branches).
- **Reading back results**, sanity-checking them against the spec,
  spotting hallucinated file paths or skipped tests, and feeding the
  next iteration if needed.
- **Reporting back to the user**, asking blocking questions,
  pushing back on bad requests, and taking responsibility for the
  final outcome.

What goes to ` + "`crush`" + ` by default:

- Anything that needs to **read** more than a couple of files.
- Anything that involves **writing or editing** code or config —
  even a "small" edit. Resist the "I'll just do it myself, it's two
  lines" instinct: those two lines come with surrounding context you
  haven't loaded.
- Running test suites, linters, type checkers, build commands.
- Searching the codebase, grepping for callers, mapping
  dependencies.
- Reproducing a bug, isolating it, drafting a fix.
- Anything repetitive or large-fan-out ("for each file matching X
  do Y").

The few legitimate exceptions for doing work yourself:

- A single ` + "`Read`" + ` of a file whose path you already know and whose
  contents you genuinely need in your own context (to talk about it
  with the user, not to act on it).
- A one-line shell command the user explicitly asked you to run.
- Reading a ` + "`crush`" + ` result file and summarising it for the user.
- **Tactical follow-up after review.** Once a ` + "`crush`" + ` run has
  returned and you've reviewed the diff/output, it is fine — and
  often the right call — to make small, surgical fixes yourself
  rather than re-delegating: a typo, a missed import, a one-line
  test tweak, a comment, a renamed variable. The rule is that the
  *bulk* implementation work was done by ` + "`crush`" + ` and your fix is
  the cherry on top, not the other way around. If the follow-up
  starts to grow past a handful of edits, that's the signal to
  hand it back to ` + "`crush`" + ` with a precise "now do X on top of what
  you just produced" prompt to the same session.

If you catch yourself reaching for ` + "`Edit`" + ` / ` + "`Write`" + ` / ` + "`Grep`" + ` / ` + "`Bash`" + `
to *start* a task rather than to finish one, pause and write a
` + "`crush run`" + ` prompt instead.

### Output format: Markdown is usually fine

**Markdown beats JSON unless something downstream parses it.**
Default to Markdown for any report a human (or another LLM acting as
orchestrator) will read. JSON only when a CI step, jq pipeline, or
fan-out harness is the immediate consumer. Wrapping prose around JSON
to "make it look structured" makes it strictly worse — readers re-flow
it, parsers reject it (validator fires ` + "`invalid_json`" + `), and you lose
both readability and parseability.

### Channels — what goes where

` + "`crush run`" + ` uses three streams. Know them before you script around it:

- **stdout**: in default (terse) mode → the final assistant text only.
  With ` + "`--stream`" + ` → every token streamed. With ` + "`--json`" + ` → one JSON
  object **at the end** — the streaming progress is swallowed by
  design, so reach for ` + "`--json`" + ` only when you actually need the
  parsed envelope (cost, tool-call counts, exit reason). Never
  tool-call traces, never spinner glyphs.
- **stderr**: tool-call heartbeat ("▶ bash", "▶ grep") + ` + "`INFO`" + `/` + "`WARN`" + `
  log lines from the agent and provider clients. Always on, regardless
  of ` + "`--quiet`" + `. (` + "`--quiet`" + ` only hides the spinner.)
- **exit code**: 0 on success or graceful cancel, non-zero on error.

### Long prompts: pipe from a file

Anything bigger than one sentence belongs in a file (use ` + "`Write`" + `),
then ` + "`< /tmp/task.md`" + ` to feed it in. Avoids quoting hell, lost
newlines, and ` + "`$VAR`" + `/backtick expansion. The file is also a handle
for re-runs against the same ` + "`--session`" + ` id. See the canonical
invocation below for the full pattern.

**Store stdin prompts under ` + "`./.crush/stdin/<task-slug>.prompt`" + `** — co-locate
them with the crush workspace data directory (` + "`.crush/`" + `) rather than
scattering them in ` + "`/tmp`" + ` or random places. The directory is
gitignored as a side-effect of the existing ` + "`.crush/`" + ` rule, so no
risk of accidentally committing. When iterating on a prompt with the same
` + "`--session`" + ` id, reuse the same file path for convenience: run once,
edit the prompt file, re-invoke with the same ` + "`crush run`" + ` command.

### Conventions

- ` + "`--role`" + ` is **required**. ` + "`smart`" + ` (or ` + "`large`" + `) for the strong/slow
  model, ` + "`fast`" + ` (or ` + "`small`" + `) for the cheap/quick one. Skipping it is
  the most common first-time failure (you'll get
  "--role is required: pass --role smart (large) or --role fast (small)").
- ` + "`--json`" + ` whenever you'll parse the result — final text, exit reason,
  per-tool call counts, token usage, duration are all on one object.
- ` + "`--session <id>`" + ` is get-or-create: pick a stable, task-meaningful id
  (issue number, branch name, feature slug). Same id continues the same
  conversation; new id starts a fresh one. The hash-prefix shortcut in
  the flag's help only applies when you're *continuing* an existing one.
- ` + "`--system-prompt-file <path>`" + ` to lock the agent into a specific role
  (reviewer, test-writer, refactorer). The prompt persists on the session
  so follow-up runs inherit it automatically.
- Permissions are **auto-approved** inside ` + "`crush run`" + ` — no human is on
  the keyboard to confirm. Don't pass ` + "`--yolo`" + ` to ` + "`crush run`" + ` (it's a
  root-level flag, not a ` + "`run`" + ` flag, and ` + "`run`" + ` already auto-approves).
  Run only in workspaces you can afford to lose, and prefer
  ` + "`--cwd /tmp/sandbox`" + ` or a worktree for risky calls.

### Shaping the model's output: ` + "`--format`" + `

When you actually need to parse the answer (not just read it), pair
` + "`--json`" + ` with ` + "`--format json`" + `. The first guarantees the wrapper-stable
envelope on stdout; the second instructs the model that ` + "`final_text`" + `
must be raw JSON with no markdown fence and no prose preamble, AND
makes ` + "`crush run`" + ` post-strip a stray ` + "```" + `json fence if the model ignored
the instruction anyway (the original wrapped text is preserved in
` + "`assistant_notes`" + ` so you can audit what the model actually said).

- ` + "`--format json`" + ` — final answer is a single raw JSON value.
- ` + "`--format json-schema:<file>`" + ` — same + conform to ` + "`<file>`" + `.
- ` + "`--format @<file>`" + ` — use ` + "`<file>`" + `'s contents verbatim as a freeform
  output-shape instruction (good for "respond in this exact template"
  prompts that don't fit on the CLI).
- ` + "`--format \"<any text>\"`" + ` — same idea, inline.

The hint is appended to the user prompt for THIS turn only — it does
not persist on the session, so a follow-up ` + "`crush run --session <same>`" + `
without ` + "`--format`" + ` reverts to the model's default verbosity.

### Sub-agent aggregation: ` + "`--aggregation`" + `

A known failure mode (measured on real audit runs): when the model
fans out via the ` + "`agent`" + ` tool and you let ` + "`--agents agent-allow`" + ` or
` + "`--agents with-agents`" + ` run, the parent often **summarises** the
sub-agent outputs into a one-paragraph wrap-up — a 7× information loss
was observed in extreme cases. The orchestrator on top sees just
` + "`final_text`" + ` and never gets the lost detail.

Three modes:

- ` + "`--aggregation summary`" + ` (default) — parent composes a wrap-up,
  sub-agent detail lives in the SQLite DB only. Cheap envelope, but
  detail loss is invisible without inspecting the DB.
- ` + "`--aggregation concat`" + ` — adds a prompt nudge asking the parent to
  include each sub-agent's reply **verbatim** in ` + "`final_text`" + `, with
  labelled section headings. Bigger ` + "`final_text`" + ` but no detail loss.
  Best when you want one big string to grep through.
- ` + "`--aggregation attach`" + ` — each sub-agent's last assistant text is
  collected by crush after the run and put into the envelope's
  ` + "`sub_agent_outputs: [{session_id, title, final_text, char_count}]`" + `
  array. ` + "`final_text`" + ` becomes a brief wrap-up. Best for machine
  consumers that want the structured set.

**Reduction-loss warning is ALWAYS on**: when the parent dispatched
≥2 sub-agents and ` + "`final_text`" + ` is <40% of the combined character
count of those sub-agents, ` + "`envelope.warnings`" + ` gets a line like
*"reduction-loss: final_text is 1421 chars (14% of 10162 combined
sub-agent chars across 3 sub-session(s))…"*. If you see that warning
in a wrapper's logs, re-run with ` + "`--aggregation=attach`" + ` (or
` + "`concat`" + `) to recover the lost detail.

If you don't pass ` + "`--aggregation`" + `, you get ` + "`summary`" + `. For audits
and report-generation tasks, **prefer ` + "`attach`" + `** — the envelope
fields keep the orchestrator's parsing logic clean.

### Sub-agent dispatch: ` + "`--agents`" + `

` + "`crush`" + ` ships with an ` + "`agent`" + ` tool (see the dedicated section below for
how it actually works). ` + "`--agents`" + ` decides whether the model can or must
use it for this run:

- ` + "`--agents single`" + ` — the ` + "`agent`" + ` and ` + "`agentic_fetch`" + ` tools are
  REMOVED from the toolset for this run. The model literally cannot
  fan out. Pick this when you want a deterministic single-path
  execution (typical for audits where you need every step in one
  session's transcript, or for cheap-and-quick tasks where fan-out
  would be overkill).
- ` + "`--agents with-agents`" + ` — the ` + "`agent`" + ` tool is present AND the user
  prompt carries a nudge telling the model "parallelise independent
  sub-tasks via ` + "`agent`" + `". Use when the task is genuinely
  decomposable (per-file scans, A/B/C alternatives, multi-section
  audits) and you want the model to actually use the fan-out.
- ` + "`--agents agent-allow`" + ` (default) — the tool is present, no
  nudge. The model decides.

If you don't pass ` + "`--agents`" + `, you get ` + "`agent-allow`" + `. State your
intent explicitly when it matters for cost/latency planning.

### Protecting your output file from the model's write tool

When you redirect ` + "`crush run > /tmp/x.json`" + `, the shell owns ` + "`x.json`" + `,
not crush. The model has a ` + "`write`" + ` tool though, and if it sees the
path in the prompt (or just decides on its own to dump findings to a
file) it may write directly to ` + "`x.json`" + ` BEFORE the envelope arrives
via stdout — leaving you with a mangled file (model's content on top,
envelope partially overwriting it because ` + "`>`" + ` open in trunc mode but
the envelope is shorter than the file already on disk).

Defence: set ` + "`CRUSH_FORBID_WRITES`" + ` to a comma-separated list of paths
the model must NOT touch via ` + "`write`" + `/` + "`edit`" + `/` + "`multiedit`" + `. The tool call
fails with a visible error to the model and it falls back to returning
the content via ` + "`final_text`" + ` — which is exactly where you wanted it.

` + "```bash" + `
out=/tmp/audit-A.json
CRUSH_FORBID_WRITES="$out" \
  crush run --role smart --json --format json \
            --session "audit-A" --timeout 15m \
            < /tmp/audit-A.prompt > "$out" 2>"$out.err"
` + "```" + `

A good general rule for the launching harness: include every
redirected file (` + "`>` `2>`" + ` targets) in ` + "`CRUSH_FORBID_WRITES`" + ` for that
run. This applies whether you' re launching one ` + "`crush run`" + ` or a
parallel fan-out — each run can have its own list.

### Read-only discovery commands (always safe)

- ` + "`crush providers list`" + ` — which providers are configured and which
  have credentials.
- ` + "`crush models show`" + ` — which model fills the smart and fast slots.
- ` + "`crush sessions list`" + ` — past conversations, with token cost.
- ` + "`crush system-prompt --session <id>`" + ` — exact prompt the next turn
  would send. Round-trip into a file, edit it, write back with
  ` + "`crush run --system-prompt-file ...`" + `.

### Expected noise at end-of-run

You'll usually see, after the answer:
` + "```" + `
WARN Failed to shutdown MCP client name=<x> error="exit status 1"
` + "```" + `
This is ` + "`crush`" + ` failing to gracefully stop the MCP servers it spawned
during the turn. The OS reaps them anyway — it's harmless, ignore it.
Anything else at WARN/ERROR level is worth a look.

### Lifecycle housekeeping

After a task ends and you don't need the context anymore:

` + "```bash" + `
crush sessions delete "<id>"     # remove session + messages
# or to retry with the same id and the same configured system prompt:
crush sessions reset  "<id>"     # wipe messages, keep id + role
` + "```" + `

### crush can fan-out inside one run — use the ` + "`agent`" + ` tool

` + "`crush`" + ` has its own ` + "`agent`" + ` tool that spawns child sessions. Describe a
decomposable task in the prompt (e.g. "for each subpackage do X",
"try approaches A/B/C and pick the best") and ` + "`crush`" + ` calls ` + "`agent`" + ` to
fan out internally — you get one consolidated answer back. Reach for
multiple parallel ` + "`crush run`" + `s instead only when the branches need
different system prompts or roles, or when you want the resulting
sessions addressable separately later.

### Background-friendly — DO NOT block on it

` + "`crush run`" + ` is long-lived. The single biggest mistake is sitting in
the foreground waiting for it, or polling with ` + "`until …; do sleep`" + ` —
both burn your context window and lock you out of the user. The harness
already knows how to wake you up when a background process exits.

**❌ Do not do these:**

- ` + "`until [ -s /tmp/out.json ]; do sleep 5; done`" + ` — polling loop
  consumes a turn per iteration and prevents the user from talking to
  you.
- ` + "`crush run … & wait`" + ` — synchronous wait on a backgrounded job is
  the same trap as foreground.
- ` + "`crush run … | tee /tmp/out`" + ` in the background — pipes race the
  shell teardown; the file ends up truncated. Always redirect with ` + "`>`" + `.
- Passing the prompt as a positional shell argument when it's larger
  than one line — quoting hell, lost newlines, embedded ` + "`$`" + ` or
  backticks expand. Use stdin instead (see below).

**✅ Do this:**

- Launch each ` + "`crush run`" + ` through ` + "`Bash`" + ` with ` + "`run_in_background: true`" + `.
  You get a task id and a rolling output file, and the harness fires a
  completion event the moment the process exits. Between launch and
  exit you talk to the user normally.
- Fan out by sending **one message with multiple ` + "`Bash`" + ` tool calls**.
  Every call runs in parallel; you get one notification per completion.
- Always redirect to files: ` + "`> /tmp/<task>.json`" + ` for the answer
  (with ` + "`--json`" + `) or ` + "`> /tmp/<task>.out`" + ` for terse text, plus
  ` + "`2>/tmp/<task>.err`" + ` for the heartbeat and any WARN/ERROR. Files
  survive context compaction — a later turn can ` + "`Read`" + ` them even if
  your scrollback got trimmed.
- For prompts above ~2 KB or anything with quotes / ` + "`$`" + ` / backticks —
  write them to a file first (` + "`Write`" + ` tool), then pipe via ` + "`< file`" + `.
- If you genuinely need a live reaction to a milestone line (e.g. wait
  for ` + "`agent: step 5`" + ` before kicking off the next job), use the
  ` + "`Monitor`" + ` tool with ` + "`tail -f /tmp/x.err | grep --line-buffered <pat>`" + `.
  Never poll with ` + "`sleep`" + `.

**Canonical patterns:**

Single fire-and-forget:
` + "```bash" + `
crush run --role smart --session "refactor-X" \
  --system-prompt-file ./prompts/refactor.md \
  --timeout 15m \
  --json \
  < /tmp/refactor-X.prompt \
  > /tmp/refactor-X.json \
  2>/tmp/refactor-X.err
` + "```" + `

Parallel fan-out (send these as **one message** with three ` + "`Bash`" + ` tool
calls, each ` + "`run_in_background: true`" + `):
` + "```bash" + `
crush run --role smart --session "approach-A" --json < /tmp/p.txt > /tmp/A.json 2>/tmp/A.err
crush run --role smart --session "approach-B" --json < /tmp/p.txt > /tmp/B.json 2>/tmp/B.err
crush run --role smart --session "approach-C" --json < /tmp/p.txt > /tmp/C.json 2>/tmp/C.err
` + "```" + `
When all three notifications have come in, ` + "`Read`" + ` the three result
files and report.

### Crash resilience: checkpointing and recovery

When ` + "`crush run`" + ` is killed (SIGTERM, OOM, timeout) during the
final streaming phase, the in-progress assistant text is NOT lost:

- **Auto-checkpoint** (always on, default 2s interval): a coalescing
  ticker flushes in-memory streaming text to the SQLite DB with a
  ` + "`Partial: true`" + ` finish marker. The marker does NOT set
  ` + "`finished_at`" + `, so recovery code can distinguish checkpoints
  from completed turns. Disable via
  ` + "`checkpoint_interval_seconds: -1`" + ` in ` + "`crush.json`" + ` options.
- **` + "`--timeout-extends-on-progress`" + `**: resets the stream watchdog
  idle deadline on every streaming event, so long compositions are not
  killed prematurely. Capped by ` + "`--timeout-hard-cap <duration>`" + `
  (typically 3-4x the idle timeout; 0 = no cap).
- **` + "`recovered_partial`" + ` envelope field**: when a subsequent
  ` + "`crush run --session <same>`" + ` detects an orphaned partial
  message, the JSON envelope includes
  ` + "`{message_id, chars, last_flush_at, text}`" + ` and a WARN in
  ` + "`warnings[]`" + `. The text may be incomplete but usually contains
  the bulk of what the model produced before the kill.
` + claudeInitMarkerEnd + `
`
}
