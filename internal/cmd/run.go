package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Aliases: []string{"r"},
	Use:     "run [prompt...]",
	Short:   "Run a single non-interactive prompt",
	Long: `Run a single prompt in non-interactive mode and exit.

  WARNING — auto-bypass of inner CLI permissions:
  ` + "`crush run`" + ` is non-interactive by design (no human at the keyboard),
  so any inner CLI sub-process it spawns (claude / codex / gemini) is
  launched with its own bypass-permissions flag (claude
  --dangerously-skip-permissions, codex --approval-mode yolo, gemini
  --yolo). The sub-process can read, write, and execute anywhere the
  invoking user has permission. There is no per-tool confirmation. Use
  ` + "`crush run`" + ` only in workspaces you can afford to lose; if you need
  real isolation, wrap the invocation in an OS-level sandbox (Sandboxie
  Plus, Windows Sandbox, WSL2 + landlock, Docker, etc.). Interactive
  sessions (TUI / web) keep the normal permission flow — this WARNING
  applies only to ` + "`crush run`" + `.

--role is REQUIRED: every invocation must declare whether it wants the
strong/slow model ("--role smart" or "--role large") or the cheap/fast
one ("--role fast" or "--role small"). The actual model id behind each
role comes from "crush models show"; --model overrides it for one
invocation. This avoids silently burning premium tokens on a one-liner.

Prompt sources (combined as "<stdin>\n\n<args>"):
  - positional args:   crush run "your prompt"
  - stdin pipe/redir:  echo "hello" | crush run     |     crush run < prompt.md
  - both (stdin = context, args = question)

Sessions: --session takes either an existing session id (or hash prefix)
to continue, OR an arbitrary new id to start a fresh session with that
exact id — handy for CI where the build matrix maps to a stable key.

System prompt: --system-prompt / --system-prompt-file persists the prompt
on the session so subsequent runs with the same --session pick it up.

Output modes (mutually exclusive --stream / --json):
  - default (terse): tool-call names on stderr as "▶ <toolName>"; only
    the final assistant message on stdout.
  - --stream:        every assistant token streamed live to stdout.
  - --json:          a single JSON object on stdout when the run ends —
                     {session_id, exit_reason, final_text, assistant_notes,
                      tool_calls, usage, duration_ms, error}. Tool-call
                     heartbeat still goes to stderr so wrappers can show
                     progress.

Shaping the model's output:
  --format <preset|string>  Appends an in-prompt hint that asks the model
                            to produce a specific shape for this turn
                            only (not persisted). Presets:
                              json              -> final answer is one
                                                   raw JSON value, no
                                                   fence, no prose.
                              json-schema:<f>   -> same + conform to <f>.
                              @<file>           -> use <file> contents
                                                   verbatim as the hint.
                              <any other text>  -> freeform "Output
                                                   format: ..." instruction.
                            With --json or --format json, the envelope's
                            final_text is also post-processed: a
                            triple-backtick "json" fence and prose
                            preamble/suffix are stripped so
                            "jq .final_text" works directly. The
                            original (unstripped) text is preserved in
                            envelope.assistant_notes when stripping ran.

Sub-agent policy:
  --agents <single|with-agents|agent-allow>
                            single        -- the "agent" and "agentic_fetch"
                                             tools are removed from the
                                             toolset for this run; the
                                             model literally cannot fan
                                             out.
                            with-agents   -- model is nudged to fan out
                                             via the "agent" tool when
                                             work is decomposable.
                            agent-allow   -- default. Tool present, model
                                             decides whether to use it.

Sub-agent aggregation (only matters when --agents permits fan-out):
  --aggregation <summary|concat|attach>
                            summary  -- default. Parent composes a
                                        wrap-up; sub-agent detail lives
                                        only in the DB. Easy on the
                                        envelope, can lose information.
                            concat   -- prompt-nudge asks the parent to
                                        include each sub-agent's reply
                                        verbatim in final_text. Bigger
                                        final_text but no detail lost.
                            attach   -- each sub-agent's last assistant
                                        text is collected into the
                                        envelope's sub_agent_outputs
                                        array; final_text becomes a
                                        brief wrap-up. Best for
                                        machine consumers that want the
                                        structured set.
                            A reduction-loss warning ALWAYS fires in
                            envelope.warnings when parent collapses
                            sub-agent outputs to <40% of their combined
                            character count.

Use --timeout to bound the run from outside (the agent gets a clean
cancel + the partial answer is preserved in the session and is included
in --json output).

Permissions: non-interactive runs auto-approve every permission request
(no one is on the keyboard to confirm). The agent gets the full tool
set with no prompting. This is fast but irreversible — only run
"crush run" in a workspace whose contents you can afford to lose, and
prefer --cwd /some/sandbox-or-temp-dir for one-shot scripts.

Protecting harness-owned files: when the orchestrator pipes "crush run"
output through a shell redirect, the model can pick the same path for
its write/edit tool (e.g. it sees ".tmp-audit-D.json" in the prompt
and decides "I'll just write findings there directly"). Result: a
mixed file where envelope and tool output overwrite each other in
non-deterministic order. To block this set the CRUSH_FORBID_WRITES
env-var to a comma-separated list of paths the write/edit/multiedit
tools must NOT touch. Example:

  out=/tmp/audit.json
  CRUSH_FORBID_WRITES="$out" \
    crush run --json --format json ... > "$out" 2>"$out.err"

The tools fail with a visible error to the model when a forbidden path
is targeted; the model then either retries with a different path or
falls back to returning the content via final_text — both of which
keep the redirect target intact.`,
	Example: `
# Run a simple prompt
crush run "Guess my 5 favorite Pokémon"

# Pipe input from stdin (stdin is prepended to the args prompt)
curl https://charm.land | crush run "Summarize this website"

# Read the prompt from a file
crush run < prompt.md

# Redirect output to a file
crush run "Generate a hot README for this project" > MY_HOT_README.md

# Quiet mode (hide the spinner) / verbose mode (show logs)
crush run --quiet  "Generate a README for this project"
crush run --verbose "Generate a README for this project"

# Continue a previous session by id (or hash prefix)
crush run --session {session-id} "Follow up on your last response"

# Continue the most recent session
crush run --continue "Follow up on your last response"

# Idempotent CI: same id across runs continues the same conversation;
# the first run creates it. Use a stable key like a PR number.
crush run --session "pr-42" "Review the latest changes"

# Override the session's system prompt from a flag
crush run --system-prompt "You are a terse senior reviewer." \
          --session "pr-42" "Review the latest changes"

# Or from a file (mutually exclusive with --system-prompt)
crush run --system-prompt-file ./reviewer-prompt.md \
          --session "pr-42" "Review the latest changes"

# Stdin user-prompt + file system-prompt + stable session id — the three
# inputs are independent, so this works as one pipeline:
git diff HEAD~1 | crush run --system-prompt-file ./reviewer-prompt.md \
                            --session "pr-42" \
                            "Review this diff"

# Required role flag — pick the strong model
crush run --role smart "design a migration for X"

# …or the cheap one for a quick one-liner
crush run --role fast "tldr this readme" < README.md

# Watch the agent think token-by-token (legacy output mode)
crush run --role smart --stream "explain this codebase"

# Machine-readable summary for wrapper scripts
crush run --json --session "pr-42" "review the diff" | jq .final_text

# Force raw JSON in final_text (model is instructed AND envelope is
# post-stripped; markdown fences and prose preamble go to
# assistant_notes).
crush run --role smart --json --format json \
          --session "audit-A" "..." < /tmp/audit-prompt.md

# JSON conforming to a schema
crush run --role smart --json \
          --format json-schema:./schemas/audit.json \
          --session "audit-A" "..." < /tmp/audit-prompt.md

# Disable sub-agent fan-out (the "agent" tool is removed from the
# toolset entirely so the model cannot dispatch even if it wants to).
crush run --role smart --agents single --session "linear-task" "..."

# Tell the model to fan out (system-prompt nudge — still up to the
# model whether it actually decomposes the work).
crush run --role smart --agents with-agents --session "parallel-audit" "..."

# Fan-out and recover every sub-agent's full text via the envelope.
# Parent's final_text becomes a brief wrap-up; sub_agent_outputs
# array carries the verbatim sub-agent replies.
crush run --role smart --json --agents with-agents --aggregation attach \
          --session "structured-audit" < /tmp/p.txt > /tmp/audit.json

# Fan-out but keep everything in final_text verbatim (one big string).
crush run --role smart --json --agents with-agents --aggregation concat \
          --session "flat-audit" < /tmp/p.txt > /tmp/audit.json

# Hard time limit — partial answer is still preserved in the session
# (and surfaced in --json's exit_reason / final_text)
crush run --timeout 5m --session "long-task" "refactor the storage layer"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			quiet, _            = cmd.Flags().GetBool("quiet")
			verbose, _          = cmd.Flags().GetBool("verbose")
			stream, _           = cmd.Flags().GetBool("stream")
			asJSON, _           = cmd.Flags().GetBool("json")
			timeout, _          = cmd.Flags().GetString("timeout")
			role, _             = cmd.Flags().GetString("role")
			effort, _           = cmd.Flags().GetString("effort")
			largeModel, _       = cmd.Flags().GetString("model")
			smallModel, _       = cmd.Flags().GetString("small-model")
			sessionID, _        = cmd.Flags().GetString("session")
			useLast, _          = cmd.Flags().GetBool("continue")
			systemPrompt, _     = cmd.Flags().GetString("system-prompt")
			systemPromptFile, _ = cmd.Flags().GetString("system-prompt-file")
			// Fork patch (orchestrator UX): --format injects a per-turn
			// output-shape hint into the user prompt; --agents controls
			// the sub-agent dispatch policy; --aggregation controls how
			// sub-agent fan-out output reaches the orchestrator. See
			// run_format.go.
			formatFlag, _      = cmd.Flags().GetString("format")
			agentsMode, _      = cmd.Flags().GetString("agents")
			aggregationMode, _ = cmd.Flags().GetString("aggregation")
			// Fork patch: batch 8 — timeout extension flags.
			timeoutExtendsOnProgress, _ = cmd.Flags().GetBool("timeout-extends-on-progress")
			timeoutHardCap, _           = cmd.Flags().GetString("timeout-hard-cap")
			// Fork patch: batch 24 — on-finish hook.
			onFinishHook, _ = cmd.Flags().GetString("on-finish")
		)

		if effort != "" {
			switch effort {
			case "low", "medium", "high":
			default:
				return fmt.Errorf("--effort: invalid value %q (allowed: low|medium|high)", effort)
			}
		}

		// --role is required so a `crush run` invocation always declares
		// its intent (cheap-and-fast vs strong-and-slow), instead of
		// silently defaulting to large and burning tokens unintentionally.
		// "smart" / "fast" are friendly aliases for "large" / "small".
		var roleLarge bool
		switch role {
		case "large", "smart":
			roleLarge = true
		case "small", "fast":
			roleLarge = false
		case "":
			return fmt.Errorf("--role is required: pass --role smart (large) or --role fast (small)")
		default:
			return fmt.Errorf("--role: invalid value %q (allowed: smart|large, fast|small)", role)
		}

		if systemPrompt != "" && systemPromptFile != "" {
			return fmt.Errorf("--system-prompt and --system-prompt-file are mutually exclusive")
		}
		if systemPromptFile != "" {
			bts, err := os.ReadFile(systemPromptFile)
			if err != nil {
				return fmt.Errorf("failed to read --system-prompt-file: %w", err)
			}
			systemPrompt = string(bts)
		}

		// Fork patch (orchestrator UX): validate --agents up-front so a
		// typo doesn't waste a billed turn. Allowed values mirror the
		// three real policies the user can want.
		var (
			agentsDisable bool
			agentsHint    string
		)
		switch agentsMode {
		case "", "agent-allow":
			// default: tool present, no nudge — model decides.
		case "with-agents":
			agentsHint = agentsModePromptHint
		case "single":
			agentsDisable = true
		default:
			return fmt.Errorf("--agents: invalid value %q (allowed: single|with-agents|agent-allow)", agentsMode)
		}

		formatHint, err := resolveFormatHint(formatFlag)
		if err != nil {
			return err
		}

		// Fork patch (orchestrator UX): validate --aggregation up-front
		// and append the matching prompt hint to the user prompt below.
		// "" / "summary" = no hint (upstream-default behaviour).
		var aggregationHint string
		switch aggregationMode {
		case "", "summary":
			// no hint
		case "concat":
			aggregationHint = aggregationConcatPromptHint
		case "attach":
			aggregationHint = aggregationAttachPromptHint
		default:
			return fmt.Errorf("--aggregation: invalid value %q (allowed: summary|concat|attach)", aggregationMode)
		}

		// Parse flexible duration flags.
		timeoutDur, err := parseDurationFlexible(timeout)
		if err != nil {
			return fmt.Errorf("--timeout: %w", err)
		}
		hardCapDur, err := parseDurationFlexible(timeoutHardCap)
		if err != nil {
			return fmt.Errorf("--timeout-hard-cap: %w", err)
		}

		// Cancel on SIGINT or SIGTERM.
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
		defer cancel()

		// Optional hard deadline. The agent run gets context.DeadlineExceeded
		// instead of context.Canceled, and the in-flight assistant message
		// finishes with FinishReasonCanceled, just like an explicit cancel.
		if timeoutDur > 0 {
			var timeoutCancel context.CancelFunc
			ctx, timeoutCancel = context.WithTimeout(ctx, timeoutDur)
			defer timeoutCancel()
		}

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		// resolveSessionID handles the lookup path (exact id / hash prefix).
		// If it fails to find anything we fall through with the raw value:
		// resolveSession's get-or-create branch will create a session
		// whose ID is exactly the user-supplied string.
		if sessionID != "" {
			if sess, err := resolveSessionID(ctx, a.Sessions, sessionID); err == nil {
				sessionID = sess.ID
			}
		}

		if !a.Config().IsConfigured() {
			return fmt.Errorf("no providers configured - please run 'crush' to set up a provider interactively")
		}

		// Fold --role into largeModel. When the user picked "fast" without
		// also passing an explicit --model, we point the agent at whatever
		// the config has saved as the small model — that's the user's
		// pre-declared "cheap/quick" choice. The agent always uses its
		// `large` slot for the turn; --role just decides which catalog
		// entry fills it.
		if !roleLarge && largeModel == "" {
			small, ok := a.Config().Models[config.SelectedModelTypeSmall]
			if !ok || small.Model == "" {
				return fmt.Errorf("--role fast: no small model configured (run \"crush models set small <model>\" first)")
			}
			largeModel = small.Provider + "/" + small.Model
		}

		if verbose {
			slog.SetDefault(slog.New(log.New(os.Stderr)))
		}

		prompt := strings.Join(args, " ")

		prompt, err = MaybePrependStdin(prompt)
		if err != nil {
			slog.Error("Failed to read from stdin", "error", err)
			return err
		}

		if prompt == "" {
			return fmt.Errorf("no prompt provided")
		}

		// Fork patch (orchestrator UX): append the format/agents/
		// aggregation hints AFTER stdin read so the user's prompt
		// content stays at the top of the model's context (attention
		// favours the start).
		prompt = composeUserPrompt(prompt, formatHint, agentsHint, aggregationHint)

		if stream && asJSON {
			return fmt.Errorf("--stream and --json are mutually exclusive")
		}
		mode := app.RunModeTerse
		switch {
		case asJSON:
			mode = app.RunModeJSON
		case stream:
			mode = app.RunModeStream
		}
		// JSON mode forces quiet (hide spinner) so the spinner glyphs don't
		// leak into stdout. The summary line on stderr we still emit.
		hideSpinner := quiet || verbose || asJSON
		overrides := app.RunOverrides{
			LargeModel:               largeModel,
			SmallModel:               smallModel,
			SystemPrompt:             systemPrompt,
			ReasoningEffort:          effort,
			RoleLarge:                roleLarge,
			DisableSubAgents:         agentsDisable,
			StripJSONFences:          formatFlag == "json" || strings.HasPrefix(formatFlag, "json-schema:"),
			AggregationMode:          aggregationMode,
			TimeoutExtendsOnProgress: timeoutExtendsOnProgress, // Fork patch: batch 8
			TimeoutHardCap:           hardCapDur,               // Fork patch: batch 8
			OnFinishHook:             onFinishHook,             // Fork patch: batch 24
		}
		return a.RunNonInteractive(ctx, os.Stdout, prompt, overrides, hideSpinner, mode, sessionID, useLast)
	},
}

func init() {
	runCmd.Flags().BoolP("quiet", "q", false, "Hide spinner")
	runCmd.Flags().BoolP("verbose", "v", false, "Show logs")
	runCmd.Flags().String("role", "", "REQUIRED. Which preselected model to use: smart|large (the strong one) or fast|small (the cheap one). The actual model id comes from `crush models show`; override with --model.")
	runCmd.Flags().String("effort", "", "Reasoning effort for this turn: low|medium|high. Applies to whichever slot --role picked. Persisted on the session so subsequent runs inherit it.")
	runCmd.Flags().Bool("stream", false, "Stream every assistant token to stdout. Default is terse: tool-call names on stderr + final answer on stdout.")
	runCmd.Flags().Bool("json", false, "Emit one JSON object on stdout summarising the run (session_id, final_text, tool_calls, usage, duration, exit_reason). Mutually exclusive with --stream.")
	runCmd.Flags().String("timeout", "0", "Abort the run after this duration (e.g. 30s, 5m, 900 — plain number = seconds). 0 = no timeout.")
	runCmd.Flags().StringP("model", "m", "", "Model to use. Accepts 'model' or 'provider/model' to disambiguate models with the same name across providers")
	runCmd.Flags().String("small-model", "", "Small model to use. If not provided, uses the default small model for the provider")
	runCmd.Flags().StringP("session", "s", "", "Session ID to continue OR create. If a session with this id exists it is continued; otherwise a new one is created with this id. Accepts a hash prefix for existing sessions only.")
	runCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	runCmd.Flags().String("system-prompt", "", "Override the session's system prompt with this string (persisted on the session)")
	runCmd.Flags().String("system-prompt-file", "", "Read the system prompt from this file (mutually exclusive with --system-prompt)")
	// Fork patch (orchestrator UX): per-turn output-shape and sub-agent
	// policy. Neither persists on the session.
	runCmd.Flags().String("format", "", "Per-turn output-shape hint appended to the user prompt. Presets: 'json' (final answer must be a single JSON value, no fences, no prose) | 'json-schema:<file>' (json + conform to this schema) | '@<file>' (use file contents verbatim as the hint) | any other text (used as a freeform 'Output format:' instruction). With --json or --format json, the envelope's final_text is also post-processed to strip ```json fences and prose preamble; the original is preserved in assistant_notes.")
	runCmd.Flags().String("agents", "", "Sub-agent dispatch policy for this run. 'single' (no sub-agents — the `agent` tool is removed from the toolset for this process) | 'with-agents' (model is nudged to fan out via the `agent` tool) | 'agent-allow' (default — tool present, model decides).")
	runCmd.Flags().String("aggregation", "", "How sub-agent fan-out output reaches the orchestrator. 'summary' (default — parent composes a wrap-up, sub-agent detail lives in DB only) | 'concat' (prompt-nudge: parent includes each sub-agent reply verbatim in final_text) | 'attach' (collect each sub-agent's last assistant text into envelope.sub_agent_outputs; final_text becomes a brief wrap-up). An always-on reduction-loss warning fires regardless when parent collapses sub-agent outputs to <40% of their combined size.")
	// Fork patch: batch 8 — timeout extension flags.
	runCmd.Flags().Bool("timeout-extends-on-progress", false, "Reset the stream watchdog deadline every time streaming progress occurs. Prevents killing healthy long compositions. Default: false (static deadline).")
	runCmd.Flags().String("timeout-hard-cap", "0", "Maximum wall-clock time the watchdog allows even with --timeout-extends-on-progress (e.g. 30s, 5m, 900 — plain number = seconds). Default: 0 (no cap).")
	// Fork patch: batch 24 — on-finish hook.
	runCmd.Flags().String("on-finish", "", "Shell command to execute after the run completes. Environment variables: CRUSH_SESSION_ID, CRUSH_EXIT_REASON, CRUSH_COST_USD, CRUSH_TOKENS, CRUSH_DURATION_SEC. Hook errors are printed to stderr but don't affect exit code.")
	runCmd.MarkFlagsMutuallyExclusive("session", "continue")
	runCmd.MarkFlagsMutuallyExclusive("system-prompt", "system-prompt-file")
	runCmd.MarkFlagsMutuallyExclusive("stream", "json")
}

// resolveSessionID resolves a session by exact UUID or hash prefix.
func resolveSessionID(ctx context.Context, svc session.Service, id string) (session.Session, error) {
	if s, err := svc.Get(ctx, id); err == nil {
		return s, nil
	}

	sessions, err := svc.List(ctx)
	if err != nil {
		return session.Session{}, err
	}

	var matches []session.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return session.Session{}, fmt.Errorf("session not found: %s", id)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "session ID '%s' is ambiguous. Matches:\n\n", id)
	for _, s := range matches {
		fmt.Fprintf(&sb, "  %s  %s\n", session.HashID(s.ID), s.Title)
	}
	return session.Session{}, fmt.Errorf("%s", sb.String())
}

// parseDurationFlexible parses a duration string that can be a plain integer
// (interpreted as seconds) or a Go duration string (e.g. "30s", "5m", "1h").
func parseDurationFlexible(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}
