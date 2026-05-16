package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Aliases: []string{"r"},
	Use:     "run [prompt...]",
	Short:   "Run a single non-interactive prompt",
	Long: `Run a single prompt in non-interactive mode and exit.

Prompt sources (combined as "<stdin>\n\n<args>"):
  - positional args:   crush run "your prompt"
  - stdin pipe/redir:  echo "hello" | crush run     |     crush run < prompt.md
  - both (stdin = context, args = question)

Sessions: --session takes either an existing session id (or hash prefix)
to continue, OR an arbitrary new id to start a fresh session with that
exact id — handy for CI where the build matrix maps to a stable key.

System prompt: --system-prompt / --system-prompt-file persists the prompt
on the session so subsequent runs with the same --session pick it up.

Output: terse by default. Tool-call names are written to stderr as
"▶ <toolName>" (one per call), and only the final assistant message is
written to stdout. Use --stream to get every assistant token in real
time. Use --timeout to bound the run from outside (the agent gets a
clean cancel + the partial answer is preserved in the session).

Permissions: non-interactive runs auto-approve every permission request
(no one is on the keyboard to confirm). The agent gets the full tool
set with no prompting. This is fast but irreversible — only run
"crush run" in a workspace whose contents you can afford to lose, and
prefer --cwd /some/sandbox-or-temp-dir for one-shot scripts.`,
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

# Watch the agent think token-by-token (legacy output mode)
crush run --stream "explain this codebase"

# Hard time limit — partial answer is still preserved in the session
crush run --timeout 5m --session "long-task" "refactor the storage layer"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			quiet, _            = cmd.Flags().GetBool("quiet")
			verbose, _          = cmd.Flags().GetBool("verbose")
			stream, _           = cmd.Flags().GetBool("stream")
			timeout, _          = cmd.Flags().GetDuration("timeout")
			largeModel, _       = cmd.Flags().GetString("model")
			smallModel, _       = cmd.Flags().GetString("small-model")
			sessionID, _        = cmd.Flags().GetString("session")
			useLast, _          = cmd.Flags().GetBool("continue")
			systemPrompt, _     = cmd.Flags().GetString("system-prompt")
			systemPromptFile, _ = cmd.Flags().GetString("system-prompt-file")
		)

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

		// Cancel on SIGINT or SIGTERM.
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
		defer cancel()

		// Optional hard deadline. The agent run gets context.DeadlineExceeded
		// instead of context.Canceled, and the in-flight assistant message
		// finishes with FinishReasonCanceled, just like an explicit cancel.
		if timeout > 0 {
			var timeoutCancel context.CancelFunc
			ctx, timeoutCancel = context.WithTimeout(ctx, timeout)
			defer timeoutCancel()
		}

		app, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer app.Shutdown()

		// resolveSessionID handles the lookup path (exact id / hash prefix).
		// If it fails to find anything we fall through with the raw value:
		// app.resolveSession's get-or-create branch will create a session
		// whose ID is exactly the user-supplied string.
		if sessionID != "" {
			if sess, err := resolveSessionID(ctx, app.Sessions, sessionID); err == nil {
				sessionID = sess.ID
			}
		}

		if !app.Config().IsConfigured() {
			return fmt.Errorf("no providers configured - please run 'crush' to set up a provider interactively")
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

		return app.RunNonInteractive(ctx, os.Stdout, prompt, largeModel, smallModel, systemPrompt, quiet || verbose, stream, sessionID, useLast)
	},
}

func init() {
	runCmd.Flags().BoolP("quiet", "q", false, "Hide spinner")
	runCmd.Flags().BoolP("verbose", "v", false, "Show logs")
	runCmd.Flags().Bool("stream", false, "Stream every assistant token to stdout. Default is terse: tool-call names on stderr + final answer on stdout.")
	runCmd.Flags().Duration("timeout", 0, "Abort the run after this duration (e.g. 30s, 5m, 1h). 0 = no timeout.")
	runCmd.Flags().StringP("model", "m", "", "Model to use. Accepts 'model' or 'provider/model' to disambiguate models with the same name across providers")
	runCmd.Flags().String("small-model", "", "Small model to use. If not provided, uses the default small model for the provider")
	runCmd.Flags().StringP("session", "s", "", "Session ID to continue OR create. If a session with this id exists it is continued; otherwise a new one is created with this id. Accepts a hash prefix for existing sessions only.")
	runCmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	runCmd.Flags().String("system-prompt", "", "Override the session's system prompt with this string (persisted on the session)")
	runCmd.Flags().String("system-prompt-file", "", "Read the system prompt from this file (mutually exclusive with --system-prompt)")
	runCmd.MarkFlagsMutuallyExclusive("session", "continue")
	runCmd.MarkFlagsMutuallyExclusive("system-prompt", "system-prompt-file")
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
