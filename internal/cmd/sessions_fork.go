package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/google/uuid"
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

		// resolveSessionID supports hash-prefix lookup, which the raw
		// db.Queries path below does not — resolve the exact source ID here,
		// then re-read it inside the transaction for a consistent snapshot.
		source, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}

		forkID, msgCount, err := forkSessionCLI(ctx, a.DB(), source.ID, forkOptions{
			atN:     atN,
			newID:   newID,
			title:   title,
			asChild: asChild,
		})
		if err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "forked session %s -> %s (copied %d messages)\n", source.ID, forkID, msgCount)
		return nil
	},
}

func init() {
	sessionsForkCmd.Flags().Int("at", 0, "Copy only the first N messages (1-indexed, default: all)")
	sessionsForkCmd.Flags().String("session", "", "ID for the new session (default: <source>-fork-<unix>)")
	sessionsForkCmd.Flags().String("title", "", "Title for the new session")
	sessionsForkCmd.Flags().Bool("child", false, "Set source as parent_session_id (creates a child session)")
}

// forkOptions carries the CLI-specific fork knobs (--at / --session / --title
// / --child) that session.Service.ForkSession does not support: ForkSession
// always copies every message into a brand-new top-level session with a
// server-generated UUID and a fixed title fallback, which is exactly right
// for the web fork button but too narrow for this command's --at truncation,
// caller-chosen ID, and --child parent linkage.
type forkOptions struct {
	atN     int
	newID   string
	title   string
	asChild bool
}

// forkSessionCLI performs the CLI fork's create-session-then-copy-messages
// operation atomically in a single DB transaction, mirroring the shape of
// session.Service.ForkSession (internal/session/session.go): a failure at
// any point — including partway through the message copy loop — rolls back
// the new session row and every message copied so far, so no half-built
// fork is left in the database for `sessions list`/`sessions show` to find.
//
// It intentionally talks to db.Queries directly rather than routing through
// session.Service / message.Service: those services either don't expose a
// transaction-scoped handle to callers (message.Service.Create always uses
// its own top-level *sql.DB) or don't support the CLI's --at/--session/
// --child knobs (session.Service.ForkSession). Message Parts are copied as
// the raw JSON string already stored in the messages table — the same
// approach ForkSession uses — so no decode/re-encode round-trip through
// message.ContentPart is needed and no unexported marshalParts helper has
// to be reused across package boundaries.
func forkSessionCLI(ctx context.Context, sqlDB *sql.DB, srcID string, opts forkOptions) (newSessionID string, copiedCount int, err error) {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("begin fork transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := db.New(tx)

	// Read the source inside the tx so the copy is consistent with itself.
	src, err := qtx.GetSessionByID(ctx, srcID)
	if err != nil {
		return "", 0, fmt.Errorf("load source session: %w", err)
	}

	srcMsgs, err := qtx.ListMessagesBySession(ctx, srcID)
	if err != nil {
		return "", 0, fmt.Errorf("list source messages: %w", err)
	}

	atN := opts.atN
	if atN == 0 {
		atN = len(srcMsgs)
	}
	if atN < 1 || atN > len(srcMsgs) {
		return "", 0, fmt.Errorf("--at %d is out of range (1..%d)", atN, len(srcMsgs))
	}
	srcMsgs = srcMsgs[:atN]

	newID := opts.newID
	if newID == "" {
		newID = fmt.Sprintf("%s-fork-%d", srcID, time.Now().Unix())
	}

	title := opts.title
	if title == "" {
		title = fmt.Sprintf("Fork of %s (at msg %d)", src.Title, atN)
	}

	createParams := db.CreateSessionParams{
		ID:                 newID,
		Title:              title,
		LargeModelProvider: src.LargeModelProvider,
		LargeModelID:       src.LargeModelID,
		SmallModelProvider: src.SmallModelProvider,
		SmallModelID:       src.SmallModelID,
	}
	if opts.asChild {
		createParams.ParentSessionID = sql.NullString{String: srcID, Valid: true}
	}
	if _, err := qtx.CreateSession(ctx, createParams); err != nil {
		return "", 0, fmt.Errorf("failed to create forked session: %w", err)
	}

	// Copy system prompt and reasoning effort, which CreateSession does not
	// accept directly (mirrors ForkSession's column-scoped follow-up UPDATEs).
	if src.SystemPrompt != "" {
		if err := qtx.UpdateSessionSystemPrompt(ctx, db.UpdateSessionSystemPromptParams{
			SystemPrompt: src.SystemPrompt,
			ID:           newID,
		}); err != nil {
			return "", 0, fmt.Errorf("copy system prompt into fork: %w", err)
		}
	}
	if src.LargeModelReasoningEffort.Valid || src.SmallModelReasoningEffort.Valid {
		if err := qtx.UpdateSessionReasoningEffort(ctx, db.UpdateSessionReasoningEffortParams{
			ID:                        newID,
			LargeModelReasoningEffort: src.LargeModelReasoningEffort,
			SmallModelReasoningEffort: src.SmallModelReasoningEffort,
		}); err != nil {
			return "", 0, fmt.Errorf("copy reasoning effort into fork: %w", err)
		}
	}

	// Copy every selected message verbatim, same raw-Parts-passthrough
	// approach as session.Service.ForkSession. Any copy error aborts the
	// whole transaction, so a failure on message N leaves neither the new
	// session row nor messages 1..N-1 behind.
	for _, m := range srcMsgs {
		if _, err := qtx.CreateMessage(ctx, db.CreateMessageParams{
			ID:                  uuid.New().String(),
			SessionID:           newID,
			Role:                m.Role,
			Parts:               m.Parts,
			Model:               m.Model,
			Provider:            m.Provider,
			ReasoningEffort:     m.ReasoningEffort,
			IsSummaryMessage:    m.IsSummaryMessage,
			Hidden:              m.Hidden,
			AutoResumed:         m.AutoResumed,
			BackgroundJobNotice: m.BackgroundJobNotice,
		}); err != nil {
			return "", 0, fmt.Errorf("failed to copy message: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("commit fork transaction: %w", err)
	}

	return newID, len(srcMsgs), nil
}
