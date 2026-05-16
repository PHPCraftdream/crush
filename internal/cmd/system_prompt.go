package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var systemPromptCmd = &cobra.Command{
	Use:   "system-prompt",
	Short: "Print the system prompt that would be sent to the model",
	Long: `Print the system prompt that crush is currently configured to send.

Without --session, prints the default prompt that crush would build for a
fresh session — i.e. the prompt baked from the active model, the registered
tools, the environment description, and the project's CRUSH.md if any.

With --session, prints the prompt persisted on that session (set previously
via "crush run --system-prompt[-file]" or the web UI). If the session has
no override yet, the default is printed instead so the output is always
the prompt that would actually be sent on the next turn.`,
	Example: `
# Print the default
crush system-prompt

# Print a specific session's prompt
crush system-prompt --session "pr-42"

# Round-trip: dump, edit, write back via "crush run --system-prompt-file"
crush system-prompt --session "pr-42" > prompt.md
$EDITOR prompt.md
crush run --system-prompt-file prompt.md --session "pr-42" "..."
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		sessionID, _ := cmd.Flags().GetString("session")

		app, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer app.Shutdown()

		if sessionID != "" {
			// Accept either an exact id or a hash prefix, same as `crush run`.
			if sess, lookupErr := resolveSessionID(ctx, app.Sessions, sessionID); lookupErr == nil {
				sessionID = sess.ID
			}
			sess, getErr := app.Sessions.Get(ctx, sessionID)
			if getErr != nil {
				return fmt.Errorf("session %q not found: %w", sessionID, getErr)
			}
			if sess.SystemPrompt != "" {
				_, _ = fmt.Fprintln(os.Stdout, sess.SystemPrompt)
				return nil
			}
			// Empty override → fall through and print the default. That way
			// `crush system-prompt --session new-id` shows what the next run
			// will actually use, not an empty string.
		}

		built, err := app.AgentCoordinator.BuildSystemPrompt(ctx)
		if err != nil {
			return fmt.Errorf("failed to build default system prompt: %w", err)
		}
		_, _ = fmt.Fprintln(os.Stdout, built)
		return nil
	},
}

func init() {
	systemPromptCmd.Flags().StringP("session", "s", "", "Print the prompt for this session (id or hash prefix). Defaults shown if the session has no override.")
	rootCmd.AddCommand(systemPromptCmd)
}
