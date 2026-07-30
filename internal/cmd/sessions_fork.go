package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/charmbracelet/crush/internal/session"
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

		// resolveSessionID supports hash-prefix lookup, which
		// session.Service.ForkSessionTx does not — resolve the exact source
		// ID here, then ForkSessionTx re-reads it inside the transaction for
		// a consistent snapshot.
		source, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}

		fork, msgCount, err := forkSessionCLI(ctx, a.Sessions, source.ID, forkOptions{
			atN:     atN,
			newID:   newID,
			title:   title,
			asChild: asChild,
		})
		if err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "forked session %s -> %s (copied %d messages)\n", source.ID, fork.ID, msgCount)
		return nil
	},
}

func init() {
	sessionsForkCmd.Flags().Int("at", 0, "Copy only the first N messages (1-indexed, default: all)")
	sessionsForkCmd.Flags().String("session", "", "ID for the new session (default: a generated UUID)")
	sessionsForkCmd.Flags().String("title", "", "Title for the new session")
	sessionsForkCmd.Flags().Bool("child", false, "Set source as parent_session_id (creates a child session)")
}

// forkOptions carries the CLI-specific fork knobs (--at / --session / --title
// / --child) that session.Service.ForkSession's zero-value defaults do not
// cover: ForkSession always copies every message into a brand-new top-level
// session with a server-generated UUID and a fixed title fallback, which is
// exactly right for the web fork button but too narrow for this command's
// --at truncation, caller-chosen ID, and --child parent linkage.
// forkSessionCLI maps these onto session.ForkOptions and calls the shared
// session.Service.ForkSessionTx.
type forkOptions struct {
	atN     int
	newID   string
	title   string
	asChild bool
}

// forkSessionCLI performs the CLI fork's create-session-then-copy-messages
// operation by delegating to session.Service.ForkSessionTx — the single
// transactional fork implementation shared with the web fork button
// (internal/session/session.go). This used to talk to db.Queries directly
// with its own copy of the transaction, but that duplicated implementation
// diverged from the web path (each independently forgot to copy a field the
// other copied: reasoning effort here, todos there). Routing through
// ForkSessionTx keeps both entry points copying the same column set and
// gives the CLI the same atomicity guarantee (a failure at any point,
// including partway through the message copy loop, rolls back the new
// session row and every message copied so far) without re-implementing it.
//
// session.Service.ForkSessionTx deliberately does not publish a
// pubsub.CreatedEvent — this CLI invocation runs in its own process, so a
// pubsub.Broker subscriber here would never be observed by whichever process
// is actually serving the web UI or another `crush run`.
func forkSessionCLI(ctx context.Context, svc session.Service, srcID string, opts forkOptions) (fork session.Session, copiedCount int, err error) {
	fork, copiedCount, err = svc.ForkSessionTx(ctx, srcID, session.ForkOptions{
		NewID:     opts.newID,
		Title:     opts.title,
		ParentID:  parentIDFor(opts, srcID),
		LimitMsgs: opts.atN,
	})
	if err != nil {
		return session.Session{}, 0, err
	}
	return fork, copiedCount, nil
}

// parentIDFor returns srcID when --child was set, else "" (top-level fork).
func parentIDFor(opts forkOptions, srcID string) string {
	if opts.asChild {
		return srcID
	}
	return ""
}
