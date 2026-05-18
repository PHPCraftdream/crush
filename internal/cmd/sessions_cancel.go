package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var sessionsCancelCmd = &cobra.Command{
	Use:   "cancel <session-id>",
	Short: "Request cancellation of a running session",
	Long: `Signal a running crush process to cancel the given session.

Sets a database flag that the running agent checks after each step. Works
across processes — use it from a second terminal or orchestrator to stop
a ` + "`crush run`" + ` that is running in the background.

The running agent will stop within one step of the flag being set.`,
	Args: cobra.MaximumNArgs(1),
	Example: `
# Cancel a specific session
crush sessions cancel my-session-id

# Cancel all sessions
crush sessions cancel --all
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		ctx := cmd.Context()

		if all {
			sessions, err := a.Sessions.List(ctx)
			if err != nil {
				return fmt.Errorf("failed to list sessions: %w", err)
			}
			count := 0
			for _, s := range sessions {
				if err := a.Sessions.RequestCancel(ctx, s.ID); err != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to cancel session %s: %v\n", s.ID, err)
					continue
				}
				count++
			}
			fmt.Fprintf(os.Stderr, "cancellation requested for %d session(s)\n", count)
			return nil
		}

		if len(args) == 0 {
			return fmt.Errorf("requires a session id or --all flag")
		}

		sess, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}
		if err := a.Sessions.RequestCancel(ctx, sess.ID); err != nil {
			return fmt.Errorf("failed to request cancellation: %w", err)
		}
		fmt.Fprintf(os.Stderr, "cancellation requested for session %s\n", sess.ID)
		return nil
	},
}

func init() {
	sessionsCancelCmd.Flags().Bool("all", false, "Cancel all top-level sessions")
}
