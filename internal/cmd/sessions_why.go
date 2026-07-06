package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsWhyCmd = &cobra.Command{
	Use:   "why <id>",
	Short: "Explain why a session has the status it has",
	Long: `Print a one-shot diagnostic explaining a session's current status
(running / crashed / done / at rest) and the evidence behind it, using
only data crush itself owns: the session/message DB and the lock file.

This is the command to reach for when "sessions list" shows a session as
"crashed" and you want to know whether it genuinely died mid-turn or
actually finished cleanly and left a stale lock behind. It does NOT read
external log files or orchestrator redirect output — only the DB and the
.crush/locks directory.

The four possible verdicts:

  done     — last assistant message finished with end_turn.
  crashed  — lock file exists, holder PID is dead, and no assistant
             message with a clean finish. Likely died mid-turn.
  running  — lock file exists, holder PID is alive. Shows heartbeat age.
  at rest  — no lock file. Not running, not crashed.

When the raw lock signal says "crashed" but the last assistant message
finished cleanly (end_turn), the verdict says so explicitly and treats
the session as done — this is the same reclassification "sessions list"
applies via reclassifyCrashedAsDone, surfaced here in plain language.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Why does sessions list show this one as crashed?
crush sessions why pr-42

# Same, by hash prefix
crush sessions why 8a3f0c
  `,
	RunE: sessionsWhyCmdRun,
}

func sessionsWhyCmdRun(cmd *cobra.Command, args []string) error {
	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	return explainSessionStatus(cmd.Context(), a, cwd, sess.ID, os.Stdout)
}

// explainSessionStatus writes a terse, plain-text explanation of why the
// session has the status it has. It is the testable core of
// `crush sessions why`: it takes the app services, the cwd (for the locks
// dir), the session id, and an output writer, so tests can drive it with a
// hand-built *app.App and a t.TempDir() without spinning up cobra.
//
// It deliberately mirrors the two-step status computation that
// `sessions list` uses (computeSessionStatuses → reclassifyCrashedAsDone)
// but for a single session, and adds the "at rest" case those helpers
// don't represent (they only return entries for sessions that HAVE a lock).
func explainSessionStatus(ctx context.Context, a *app.App, cwd, sessionID string, out io.Writer) error {
	msgs, err := a.Messages.List(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages for session %s: %w", sessionID, err)
	}

	// Lock state — same path / parse logic as computeSessionStatuses and
	// sessionsLocksCmdRun, but for the single session we care about.
	lockPath := filepath.Join(cwd, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	var (
		hasLock   bool
		pid       int
		pidAlive  bool
		heartAge  time.Duration
	)
	if st, statErr := os.Stat(lockPath); statErr == nil {
		hasLock = true
		pid = session.ReadLockPID(lockPath)
		pidAlive = pid > 0 && session.IsProcessAlive(pid)
		heartAge = time.Since(st.ModTime())
	}

	// Last assistant message + its finish part (if any). Same
	// reverse-scan as reclassifyCrashedAsDone.
	var lastAssistant *message.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant {
			lastAssistant = &msgs[i]
			break
		}
	}
	var finish *message.Finish
	if lastAssistant != nil {
		finish = lastAssistant.FinishPart()
	}

	// Verdict + reason text. Four cases, matching the Long help above.
	// "at rest" (no lock) is the one computeSessionStatuses can't express.
	switch {
	case !hasLock:
		fmt.Fprintf(out, "status: at rest\n")
		fmt.Fprintf(out, "reason: no lock file present — not running, not crashed.\n")
		if finish != nil && finish.Reason == message.FinishReasonEndTurn {
			fmt.Fprintf(out, "last assistant message finished cleanly (end_turn); session is idle.\n")
		} else if lastAssistant == nil {
			fmt.Fprintf(out, "no assistant message recorded yet.\n")
		} else {
			fmt.Fprintf(out, "last assistant message did not finish cleanly (%s).\n", finishReasonOrUnknown(finish))
		}
	case hasLock && pidAlive:
		fmt.Fprintf(out, "status: running\n")
		fmt.Fprintf(out, "reason: lock held by live PID %d (heartbeat %s old).\n", pid, formatDurationShort(heartAge))
		if finish != nil {
			fmt.Fprintf(out, "last assistant finish: %s\n", finishReasonOrUnknown(finish))
		} else {
			fmt.Fprintf(out, "last assistant finish: (none yet — turn in progress)\n")
		}
	default:
		// hasLock && !pidAlive → raw signal is "crashed". Decide whether
		// the message store contradicts that (clean end_turn → really
		// "done") — this is the reclassifyCrashedAsDone rule.
		fmt.Fprintf(out, "status: crashed\n")
		fmt.Fprintf(out, "reason: lock file exists but holder PID %d is not alive.\n", pid)
		if finish != nil && finish.Reason == message.FinishReasonEndTurn {
			fmt.Fprintf(out, "\n")
			fmt.Fprintf(out, "NOTE: the last assistant message finished cleanly (end_turn).\n")
			fmt.Fprintf(out, "This is likely a stale lock from a process that exited without\n")
			fmt.Fprintf(out, "cleanup, or another process finished this session concurrently.\n")
			fmt.Fprintf(out, "Treat as done.\n")
		} else if lastAssistant == nil {
			fmt.Fprintf(out, "no assistant message with a clean finish — likely died mid-turn.\n")
		} else {
			fmt.Fprintf(out, "no clean finish found (last finish: %s) — likely died mid-turn.\n", finishReasonOrUnknown(finish))
		}
	}

	// Always surface the raw last-assistant finish reason + error text if
	// present, so the operator sees the underlying signal regardless of
	// which branch above fired.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Last assistant message:")
	if lastAssistant == nil {
		fmt.Fprintln(out, "  (none)")
	} else {
		fmt.Fprintf(out, "  finish_reason: %s\n", finishReasonOrUnknown(finish))
		if finish != nil && finish.Reason == message.FinishReasonError {
			errText := finish.Message
			if errText == "" {
				errText = "(error finish reason but no error text stored)"
			}
			fmt.Fprintf(out, "  error:         %s\n", errText)
		}
	}

	return nil
}

// finishReasonOrUnknown returns the finish reason string, or "(unknown)"
// when there is no Finish part at all (message never finished).
func finishReasonOrUnknown(f *message.Finish) string {
	if f == nil || f.Reason == "" {
		return "(unknown)"
	}
	return string(f.Reason)
}
