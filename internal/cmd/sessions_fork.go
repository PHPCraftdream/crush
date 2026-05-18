package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsForkCmd = &cobra.Command{
	Use:   "fork <source-id>",
	Short: "Fork a session, copying its messages into a new session",
	Long: `Create a new session whose messages are a copy of the source
session's first N messages (inclusive). Useful for branching a conversation
from a particular point without modifying the original.

The new session is a top-level session (no parent) unless --child is set.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Fork all messages into a new session
crush sessions fork my-session-id

# Fork only the first 5 messages
crush sessions fork my-session-id --at 5

# Fork with a custom session id and title
crush sessions fork my-session-id --session new-id --title "My Fork"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		atN, _ := cmd.Flags().GetInt("at")
		newID, _ := cmd.Flags().GetString("session")
		title, _ := cmd.Flags().GetString("title")
		asChild, _ := cmd.Flags().GetBool("child")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		ctx := cmd.Context()

		source, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}

		msgs, err := a.Messages.List(ctx, source.ID)
		if err != nil {
			return fmt.Errorf("failed to list messages: %w", err)
		}

		// Determine cutoff.
		if atN == 0 {
			atN = len(msgs)
		}
		if atN < 1 || atN > len(msgs) {
			return fmt.Errorf("--at %d is out of range (1..%d)", atN, len(msgs))
		}
		msgs = msgs[:atN]

		// Generate new session ID.
		if newID == "" {
			newID = fmt.Sprintf("%s-fork-%d", source.ID, time.Now().Unix())
		}

		// Generate title.
		if title == "" {
			title = fmt.Sprintf("Fork of %s (at msg %d)", source.Title, atN)
		}

		// Create the new session.
		var newSess session.Session
		if asChild {
			newSess, err = a.Sessions.CreateTaskSession(ctx, newID, source.ID, title)
		} else {
			newSess, err = a.Sessions.CreateWithID(ctx, newID, title)
		}
		if err != nil {
			return fmt.Errorf("failed to create forked session: %w", err)
		}

		// Copy model settings from source.
		if source.LargeModelID != "" || source.SmallModelID != "" {
			if err := a.Sessions.UpdateModels(ctx, newSess.ID,
				source.LargeModelProvider, source.LargeModelID,
				source.SmallModelProvider, source.SmallModelID,
			); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to copy model settings: %v\n", err)
			}
		}
		if source.LargeModelReasoningEffort != "" || source.SmallModelReasoningEffort != "" {
			if err := a.Sessions.UpdateReasoningEffort(ctx, newSess.ID,
				source.LargeModelReasoningEffort, source.SmallModelReasoningEffort,
			); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to copy reasoning effort: %v\n", err)
			}
		}
		if source.SystemPrompt != "" {
			if err := a.Sessions.UpdateSystemPrompt(ctx, newSess.ID, source.SystemPrompt); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to copy system prompt: %v\n", err)
			}
		}

		// Copy messages.
		for _, msg := range msgs {
			params := message.CreateMessageParams{
				Role:             msg.Role,
				Parts:            msg.Parts,
				Model:            msg.Model,
				Provider:         msg.Provider,
				ReasoningEffort:  msg.ReasoningEffort,
				IsSummaryMessage: msg.IsSummaryMessage,
				Hidden:           msg.Hidden,
			}
			if _, err := a.Messages.Create(ctx, newSess.ID, params); err != nil {
				return fmt.Errorf("failed to copy message: %w", err)
			}
		}

		fmt.Fprintf(os.Stderr, "forked session %s -> %s (copied %d messages)\n", source.ID, newSess.ID, len(msgs))
		return nil
	},
}

func init() {
	sessionsForkCmd.Flags().Int("at", 0, "Copy only the first N messages (1-indexed, default: all)")
	sessionsForkCmd.Flags().String("session", "", "ID for the new session (default: <source>-fork-<unix>)")
	sessionsForkCmd.Flags().String("title", "", "Title for the new session")
	sessionsForkCmd.Flags().Bool("child", false, "Set source as parent_session_id (creates a child session)")
}
