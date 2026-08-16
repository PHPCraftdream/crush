package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List, observe, and manage sessions — the full orchestration toolkit",
	Long: `Sessions are the unit of conversation context. The web UI and
"crush run" both create / continue them. This subcommand gives full
CLI access to the session store for scripting, orchestration, and debugging.

Core:        list (with STATUS column), show (with purpose + budget), delete, reset (--force)
Observe:     last (with timestamps), tail --follow, locks (heartbeat + budget),
             watch (live dashboard), pick (interactive TUI)
Search:      grep <pattern> (message text), diff <id> (files touched),
             cost [--by model|day|session] (spend breakdown)
Orchestrate: cancel <id> (graceful DB-flag stop), fork <id> [--at N],
             tree (parent-child hierarchy), gc (garbage-collect stale)
Cleanup:     purge <age> [--matching <glob>], kill <id> (force-unlock),
             reap (remove all orphan locks)`,
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all top-level sessions",
	Long: `List all top-level (non-child) sessions in this workspace.

Without --json the output is a fixed-width table; with --json each line is
one JSON object suitable for jq / streaming consumers.`,
	Example: `
# Human-readable table
crush sessions list

# Machine-readable (one object per line)
crush sessions list --json | jq 'select(.message_count > 0)'
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		sessions, err := a.Sessions.List(cmd.Context())
		if err != nil {
			return fmt.Errorf("failed to list sessions: %w", err)
		}

		// Filter out internal child sessions (sub-agents, title-generators).
		visible := sessions[:0]
		for _, s := range sessions {
			if s.ParentSessionID != "" {
				continue
			}
			visible = append(visible, s)
		}
		sessions = visible

		// Fork patch (orchestrator UX, round 2 #1): compute STATUS by
		// reading the locks directory once. running = lock exists and
		// holder PID is alive; crashed = lock exists but PID is dead
		// (will be auto-reclaimed on next acquire); blank = no lock,
		// session is at rest. The lock dir read is one syscall + N
		// directory entries; the PID liveness check is the same cheap
		// per-PID probe `sessions reap` uses.
		statusByID := computeSessionStatuses(a)

		// A dead-PID lock can mean two things: a genuine mid-turn crash,
		// or a `crush run` that finished cleanly (last assistant turn
		// ended with end_turn) and exited within the ~60s heartbeat
		// sweep window — its lock file is still on disk but the PID is
		// gone. Reclassify those to "done" so a clean exit isn't shown
		// as "crashed". Cheap: only the handful of sessions with a stale
		// lock actually hit the message store.
		statusByID = reclassifyCrashedAsDone(cmd.Context(), a, sessions, statusByID)

		// Sub-agent awareness: a "running" session that is currently blocked
		// inside an `agent` delegation gets promoted to "delegating" so the
		// STATUS column distinguishes "top-level agent is working" from "top-
		// level agent is waiting on a sub-agent". The freshness signal comes
		// from the shared call-tree walk (sessions_activity.go), NOT from the
		// lock mtime, so it reflects the sub-agent actually making progress.
		statusByID = markDelegatingSessions(cmd.Context(), a, sessions, statusByID)

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, s := range sessions {
				item := makeSessionListItem(s)
				if st := statusByID[s.ID]; st != "" {
					item.Status = st
				}
				if err := enc.Encode(item); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "HASH\tID\tTITLE\tMSGS\tSTATUS\tUPDATED\tTOKENS\tCOST")
		for _, s := range sessions {
			fmt.Fprintf(
				tw, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t$%.4f\n",
				short(session.HashID(s.ID)),
				s.ID,
				truncate(s.Title, 40),
				s.MessageCount,
				statusOrDash(statusByID[s.ID]),
				time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04"),
				s.PromptTokens+s.CompletionTokens,
				s.Cost,
			)
		}
		return tw.Flush()
	},
}

// computeSessionStatuses returns sessionID → status ("running" | "crashed").
// Sessions not in the map are at rest (no lock). Cheap: one directory read +
// one PID probe per lock file.
//
// A session counts as "running" if EITHER the PID it recorded is alive OR
// its heartbeat (lock file mtime) is still fresh. The PID check alone is
// not reliable on Windows: tryLockFile takes a mandatory, whole-file
// LockFileEx lock for the holder's entire lifetime, so a plain read of the
// PID from another process fails for as long as the session is alive (see
// the Windows note on session.readLockFile) — without the heartbeat
// fallback, every live session on Windows would misreport as "crashed".
//
// Takes the already-booted *app.App (rather than re-deriving the data
// directory from --cwd) so it honors --data-dir / a configured
// data_directory the same way `sessions locks` does — see task #233,
// the same cwd-hardcoding bug class as task #219/#224/#231.
//
// Deliberately does NOT delegate to session.InspectSessionLock (unlike
// sessions_watch.go's isSessionFinished, task #241): InspectSessionLock's
// shape is "mtime fresh is the fast path; only when mtime already looks
// stale, fall back to probing the PID". This function's shape is the
// opposite — a CONFIRMED pid > 0 is trusted unconditionally, mtime is only
// the fallback for an unreadable/ambiguous PID (pid <= 0, the Windows norm
// for a live session) — so the two are not interchangeable without
// changing behavior here. The pid > 0 branch below is bounded by
// session.MaxPidFallbackAge for the same reason InspectSessionLock is
// (task #235/#241): without a bound, a lock abandoned by a killed/crashed
// `crush run` whose recorded PID the OS later recycles for an unrelated,
// currently-running process would report "running" forever, with
// `sessions list`'s STATUS column never self-healing to "crashed"/"done".
func computeSessionStatuses(a *app.App) map[string]string {
	if a == nil {
		return nil
	}
	locksDir := filepath.Join(a.Config().Options.DataDirectory, "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, ".lock") {
			continue
		}
		sessionID := strings.TrimSuffix(strings.TrimPrefix(name, "session-"), ".lock")
		path := filepath.Join(locksDir, name)
		pid := session.ReadLockPID(path)
		info, statErr := entry.Info()
		// A CONFIRMED-dead PID (pid > 0 but not alive) is trustworthy on
		// its own. pid <= 0 is ambiguous — "unreadable", not necessarily
		// dead (see the Windows note on session.readLockFile) — so only
		// then do we fall back to heartbeat freshness. A CONFIRMED-alive
		// PID is trusted only within session.MaxPidFallbackAge of the
		// lock's mtime — past that, the lock is old enough that no
		// genuinely healthy holder could still be running it, so a
		// currently-alive PID is far more likely to be an OS PID-reuse
		// coincidence than the original holder (task #241).
		var alive bool
		switch {
		case pid > 0 && statErr == nil && time.Since(info.ModTime()) >= session.MaxPidFallbackAge:
			alive = false
		case pid > 0:
			alive = session.IsProcessAlive(pid)
		case statErr == nil:
			alive = time.Since(info.ModTime()) <= session.LockStaleDuration
		}
		if alive {
			out[sessionID] = "running"
		} else {
			out[sessionID] = "crashed"
		}
	}
	return out
}

// reclassifyCrashedAsDone promotes a "crashed" status to "done" when the
// session's last ASSISTANT message finished cleanly (FinishReasonEndTurn).
// A dead-PID lock without such a clean finish stays "crashed" — that's the
// genuine mid-turn-crash case. Mutates and returns statusByID in place so
// both the JSON and table render paths share the same corrected map.
//
// Only sessions currently flagged "crashed" hit the message store, so the
// cost is proportional to the number of stale locks (usually zero or one).
func reclassifyCrashedAsDone(
	ctx context.Context,
	a *app.App,
	sessions []session.Session,
	statusByID map[string]string,
) map[string]string {
	if statusByID == nil || a == nil {
		return statusByID
	}
	for _, s := range sessions {
		if statusByID[s.ID] != "crashed" {
			continue
		}
		msgs, err := a.Messages.List(ctx, s.ID)
		if err != nil {
			continue
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != message.Assistant {
				continue
			}
			if msgs[i].FinishReason() == message.FinishReasonEndTurn {
				statusByID[s.ID] = "done"
			}
			break
		}
	}
	return statusByID
}

// markDelegatingSessions promotes a "running" status to "delegating" when
// the session's freshest activity is coming from an in-flight sub-agent
// delegation rather than the top-level agent itself. This is the STATUS-
// column consumer of the shared call-tree activity signal: it lets an
// operator scanning `sessions list` see at a glance which running sessions
// are currently blocked on (and being kept alive by) a sub-agent.
//
// Only sessions already flagged "running" are probed — at-rest / crashed /
// done sessions are left untouched. All of them are checked in ONE batched
// SQL query (computeCallTreeActivityBatch) instead of one call-tree query
// per running session, so `sessions list` stays O(1) queries for this step
// regardless of how many sessions happen to be running concurrently.
func markDelegatingSessions(
	ctx context.Context,
	a *app.App,
	sessions []session.Session,
	statusByID map[string]string,
) map[string]string {
	if statusByID == nil || a == nil {
		return statusByID
	}

	running := make([]session.Session, 0, len(sessions))
	for _, s := range sessions {
		if statusByID[s.ID] == "running" {
			running = append(running, s)
		}
	}
	if len(running) == 0 {
		return statusByID
	}

	ids := make([]string, len(running))
	for i, s := range running {
		ids[i] = s.ID
	}
	activity := computeCallTreeActivityBatch(ctx, a, ids)

	for _, s := range running {
		act, ok := activity[s.ID]
		if !ok {
			continue
		}
		// Baseline = the session's own updated_at. A descendant sub-agent
		// message newer than that means the live edge of work is inside a
		// delegation. (The session row's updated_at is NOT bumped by child
		// message inserts — see the DB triggers — so this comparison is
		// meaningful.)
		if act.LatestUnix > s.UpdatedAt && act.SubAgentActive {
			statusByID[s.ID] = "delegating"
		}
	}
	return statusByID
}

func statusOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var sessionsDeleteCmd = &cobra.Command{
	Use:     "delete <id>",
	Aliases: []string{"rm"},
	Short:   "Delete a session and all its messages",
	Long: `Permanently delete a session row and every message attached to it.

The <id> can be a full session id or a hash prefix (as printed by
"sessions list"). Use this to clean up scratch sessions created during
experiments — for example, the per-PR ids that "crush run --session pr-NN"
leaves behind after the work is merged.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush sessions delete pr-42
crush sessions delete 8a3f0c  # match by hash prefix
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		return deleteSessionByIDOrHash(cmd.Context(), a, args[0])
	},
}

var sessionsResetCmd = &cobra.Command{
	Use:   "reset <id>",
	Short: "Drop a session's messages but keep its id, title, and system prompt",
	Long: `Wipe the conversation history of a session while preserving the
session row itself — including its id, title, persisted system prompt,
and per-session model selection.

Useful when you want to re-run "crush run --session <same-id>" from a
clean slate without picking a new id and losing the side-channel state
(system prompt, model overrides) that you previously configured.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Wipe history, keep system prompt, continue with same id
crush sessions reset pr-42
crush run --session pr-42 "try again with the fresh context"

# Reset even if a stale lock from a crashed process is in the way
crush sessions reset pr-42 --force
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
		if err != nil {
			return err
		}

		// Fork patch (orchestrator UX): --force kills any process still
		// holding the session's lock and removes the lock file. Without
		// this, a reset can succeed at the DB level but a subsequent
		// `crush run --session <same>` still fails with "session is
		// already in use" because the previous holder crashed without
		// releasing.
		//
		// Uses the shared probeThenKillHolder + removeLockWithRetry
		// helpers (defined in sessions_kill.go) so kill / wait-for-death /
		// retry-remove behaves identically here and in `sessions kill`:
		// probeThenKillHolder first attempts a real OS-level lock
		// acquisition before trusting the PID recorded in the lock file,
		// so a stale/recycled PID from an already-exited holder is never
		// blindly killed. On Windows the kill (when a live holder is
		// actually found) goes through taskkill /F /T which also
		// terminates the spawned CLI subprocess tree.
		if force {
			// Use the data directory setupApp already resolved onto `a`
			// (honors --data-dir and the project's configured
			// data_directory) instead of recomputing a cwd-based guess —
			// see task #219.
			dataDir := a.Config().Options.DataDirectory
			lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
			fmt.Fprintf(os.Stderr, "reset --force: acquiring session lock at %s\n", lockPath)
			pid := session.ReadLockPID(lockPath)
			lk, kr, acquireErr := acquireSessionLockForReset(dataDir, sess.ID, pid, 5*time.Second)
			if acquireErr != nil {
				fmt.Fprint(os.Stderr, kr.Report)
				return fmt.Errorf("reset --force: %w", acquireErr)
			}
			fmt.Fprint(os.Stderr, kr.Report)
			// HOLD the real OS lock across the DB reset below so no concurrent
			// `crush run --session <id>` can recreate the lock at this path and
			// start writing into the session DB while the wipe is in flight.
			// The lock FILE is deliberately NOT removed: an empty lock file
			// with no held OS lock is harmless (the next acquirer reopens and
			// overwrites it; see internal/session/lock.go's Release), and
			// removing a path a live holder may be reusing is exactly the
			// two-owners bug this command must avoid.
			defer lk.Release()
		}

		if err := a.Messages.DeleteSessionMessages(cmd.Context(), sess.ID); err != nil {
			return fmt.Errorf("failed to reset session %s: %w", sess.ID, err)
		}
		// Zero the per-session usage counters so a follow-up run starts
		// from an honest "empty context" estimate.
		//
		// Fork patch (concurrency): cost is mutated only through
		// IncrementCost now — Save no longer writes the column. Zero it
		// by applying a negative delta equal to the current value. See
		// CHANGELOG.fork.md (Section 4.I).
		previousCost := sess.Cost
		if err := a.Sessions.SetSummaryAndUsage(cmd.Context(), sess.ID, "", 0, 0); err != nil {
			return fmt.Errorf("failed to reset session counters for %s: %w", sess.ID, err)
		}
		if previousCost != 0 {
			if _, err := a.Sessions.IncrementCost(cmd.Context(), sess.ID, -previousCost); err != nil {
				return fmt.Errorf("failed to reset session cost for %s: %w", sess.ID, err)
			}
		}
		fmt.Fprintf(os.Stderr, "reset session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
		return nil
	},
}

var sessionsShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Inspect a single session in detail",
	Long: `Show detailed information about a session including its title, models,
tokens, cost, and optionally all messages.

The default output is human-readable text; use --json for structured format
suitable for parsing. Combine with --with-messages to include the message
thread and system prompt. Use --full with --with-messages to see complete
message content (default truncates to 200 chars per message).`,
	Args: cobra.ExactArgs(1),
	Example: `
# Human-readable inspection
crush sessions show myid-123

# Full session data with all messages
crush sessions show myid-123 --with-messages

# Machine-readable format for scripts
crush sessions show myid-123 --json

# See everything including full message content
crush sessions show myid-123 --with-messages --full --json
  `,
	RunE: sessionsShowCmdRun,
}

var sessionsLocksCmd = &cobra.Command{
	Use:   "locks",
	Short: "List active session lock files",
	Long: `Scan the .crush/locks directory for session lock files and report
their status: session id, PID, when the lock was acquired, and whether
it appears stale (process not running or lock older than 10 minutes).

Lock files are typically acquired when a session is running and released
when the run completes. Stale locks can accumulate if processes crash
without cleanup. By default this command is READ-ONLY: it lists locks and
never removes anything — an empty lock file with no held OS lock is
harmless (the next acquirer reopens and overwrites it; see
internal/session/lock.go's Release), so auto-deleting stable per-session
lock files is unnecessary and only ever reintroduces TOCTOU "unlink a path
a live holder may be reusing" bugs.

Pass --prune to opt in to removing a lock once it has been proven (via a
real OS-level lock probe, not just its mtime age) that no live process
actually holds it. Entries that merely LOOK stale by mtime (older than 60s)
but that a real OS-level lock probe proves are still held are never deleted
— only a lock file with no live OS-level holder is removed under --prune.
See lockHolderProvablyDead's doc comment for the full discipline and its
two documented, narrow residual windows (a post-probe TOCTOU on removal,
and brief probe-induced contention with a concurrent "crush run" starting
on the exact same session id). --prune is opt-in precisely because those
narrow windows are accepted for an explicit operator request, not for a
default that runs silently on every invocation.

Use --stale-only to filter to suspicious locks. Use --json for NDJSON
output suitable for metrics collection or automation.`,
	Example: `
# Show all locks
crush sessions locks

# Show only stale locks
crush sessions locks --stale-only

# Stream to jq for scripting
crush sessions locks --json | jq '.session_id'
  `,
	RunE: sessionsLocksCmdRun,
}

var sessionsTailCmd = &cobra.Command{
	Use:   "tail <id>",
	Short: "Stream messages from a session",
	Long: `Output messages from a session, one block per message. By default,
prints all messages and exits. With --follow, polls for new messages
until the session finishes (last message has a non-Partial finish reason)
or until you press Ctrl+C.

Use --from-message <id> to resume from a specific message (skips earlier
messages). Use --format ndjson to emit JSON per line for piping into jq
or other tools.

Exit codes:
  0 — session completed or user interrupted with Ctrl+C
  1 — session not found
  2 — database error while streaming
  `,
	Args: cobra.ExactArgs(1),
	Example: `
# Print all messages and exit
crush sessions tail myid-123

# Live-tail a running session (Ctrl+C to stop)
crush sessions tail myid-123 --follow

# Resume from message abc123 in NDJSON format
crush sessions tail myid-123 --from-message abc123 --format ndjson

# Pipe to jq for filtering
crush sessions tail myid-123 --format ndjson | jq '.role == "assistant"'
  `,
	RunE: sessionsTailCmdRun,
}

var sessionsLastCmd = &cobra.Command{
	Use:   "last <id>",
	Short: "Show the last N messages of a session",
	Long: `Print the most recent messages from a session without following.
Useful for quickly checking what an agent produced.

Use --n to control how many messages to show (default 10).
Use --format ndjson for machine-readable output.`,
	Example: `
# Show last 10 messages
crush sessions last myid-123

# Show last 3 messages
crush sessions last myid-123 --n 3

# Machine-readable
crush sessions last myid-123 --format ndjson | jq '.role'
`,
	Args: cobra.ExactArgs(1),
	RunE: sessionsLastCmdRun,
}

func sessionsLastCmdRun(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetInt("n")
	format, _ := cmd.Flags().GetString("format")
	withSubagents, _ := cmd.Flags().GetBool("with-subagents")
	if format != "text" && format != "ndjson" {
		return fmt.Errorf("invalid format: %s (must be text or ndjson)", format)
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	// Fix (pre-existing): resolveSessionID returned the full session but
	// the next call passed args[0] (which may be a short hash). On a
	// short-hash invocation that meant Messages.List got the hash, no
	// match, empty output. Use the resolved ID.
	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	messages, err := a.Messages.List(cmd.Context(), sess.ID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if len(messages) > n {
		messages = messages[len(messages)-n:]
	}
	// Build the tool-call context from the FULL message list (not just
	// the trimmed window) so a ToolResult inside the window can still
	// look up its matching ToolCall that may have been emitted earlier.
	callCtx := buildToolCallContext(messages)
	now := time.Now()
	// The window always ends at the true tail of the session (we trim from
	// the front), so a row is "followed by a later message" iff it isn't the
	// last one in the window. That's the signal finishReasonLabel uses to
	// tell a transient, auto-retried error from a terminal one.
	for i, msg := range messages {
		printMessageWithTime(os.Stdout, msg, format, now, callCtx, i < len(messages)-1)
	}

	// Opt-in (--with-subagents): after the parent's own stream, render each
	// sub-agent delegation's full transcript as a demarcated, indented block.
	// Default-hidden — without the flag we print only the parent rows (plus
	// the one-line pulse note below), never a child's message content inline.
	if withSubagents {
		printSubAgentTranscripts(cmd.Context(), os.Stdout, a, sess.ID, format, now)
	}

	// Sub-agent pulse: `last` shows only the TOP-LEVEL session's rows, so an
	// in-flight `agent` delegation (which writes to a child session) is
	// invisible here. Append a one-line note when the freshest activity in
	// the call tree is a sub-agent's, so `last` doesn't look frozen while a
	// delegation is actively running. Text format only — ndjson consumers
	// get the structured signal from `sessions locks --json` / `show --json`.
	if format == "text" {
		if note := subAgentActivityNote(cmd.Context(), a, sess.ID, sess.UpdatedAt, now); note != "" {
			fmt.Fprintf(os.Stdout, "[%s]\n\n", note)
		}
	}
	return nil
}

func init() {
	sessionsListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")

	sessionsResetCmd.Flags().Bool("force", false, "Also kill any process holding the session lock and remove the lock file")

	sessionsShowCmd.Flags().Bool("json", false, "Emit structured JSON instead of text")
	sessionsShowCmd.Flags().Bool("with-messages", false, "Include all messages in the output")
	sessionsShowCmd.Flags().Bool("full", false, "Show full message content (implies --with-messages)")
	sessionsShowCmd.Flags().Bool("with-subagents", false, "Also render each sub-agent delegation's transcript as a demarcated block (implies --with-messages; text output)")

	sessionsLocksCmd.Flags().Bool("json", false, "Emit NDJSON (one JSON object per line)")
	sessionsLocksCmd.Flags().Bool("stale-only", false, "Filter to locks older than 10 minutes or for dead processes")
	sessionsLocksCmd.Flags().Bool("prune", false, "Remove lock files proven (via a real OS-level lock probe) to have no live holder. Off by default: `sessions locks` is read-only unless this is set.")

	sessionsTailCmd.Flags().Bool("follow", false, "Keep polling for new messages until session finishes")
	sessionsTailCmd.Flags().String("from-message", "", "Resume from this message ID (skip earlier)")
	sessionsTailCmd.Flags().String("format", "text", "Output format: text or ndjson")
	sessionsTailCmd.Flags().Bool("with-subagents", false, "After the parent stream, render each sub-agent delegation's transcript as a demarcated block (snapshot; not re-emitted while --follow)")

	sessionsLastCmd.Flags().IntP("n", "n", 10, "Number of messages to show")
	sessionsLastCmd.Flags().String("format", "text", "Output format: text or ndjson")
	sessionsLastCmd.Flags().Bool("with-subagents", false, "After the parent messages, render each sub-agent delegation's transcript as a demarcated block")

	sessionsCacheCmd.Flags().Bool("json", false, "Emit JSON instead of a table")
	sessionsCacheCmd.Flags().String("since", "", "Only count messages newer than this: Go duration (30m, 24h), day suffix (7d), or a bare integer read as days. Aggregates across all sessions; omit the session argument.")
	sessionsCacheCmd.Flags().String("by", "", "Grouping for the cross-session view: model (default) or day")

	sessionsCmd.AddCommand(sessionsListCmd, sessionsDeleteCmd, sessionsResetCmd, sessionsShowCmd, sessionsLocksCmd, sessionsTailCmd, sessionsLastCmd, sessionsWhyCmd, sessionsGcCmd, sessionsPurgeCmd, sessionsKillCmd, sessionsReapCmd, sessionsWatchCmd, sessionsPickCmd, sessionsGrepCmd, sessionsCostCmd, sessionsCacheCmd, sessionsDiffCmd, sessionsCancelCmd, sessionsForkCmd, sessionsTreeCmd)
	rootCmd.AddCommand(sessionsCmd)
}

// sessionListItem is the JSON shape of `crush sessions list --json`. Held
// as a separate struct (rather than just marshalling session.Session
// directly) so the wire-stable field names don't drift if session.Session
// gains internal fields we don't want to publish.
type sessionListItem struct {
	ID           string  `json:"id"`
	Hash         string  `json:"hash"`
	Title        string  `json:"title"`
	MessageCount int64   `json:"message_count"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
	Tokens       int64   `json:"tokens"`
	CostUSD      float64 `json:"cost_usd"`
	YoloEnabled  bool    `json:"yolo_enabled"`
	// Status is "running" (lock exists, holder PID alive), "crashed"
	// (lock exists but PID dead — will be auto-reclaimed) or "" (at rest).
	// Computed live from the locks directory at list time. omitempty so
	// the field is absent for at-rest sessions, keeping the wire shape
	// minimal for the common case.
	Status string `json:"status,omitempty"`
}

// makeSessionListItem projects a session.Session into the wire-stable
// sessionListItem shape used by `crush sessions list --json`.
func makeSessionListItem(s session.Session) sessionListItem {
	return sessionListItem{
		ID:           s.ID,
		Hash:         session.HashID(s.ID),
		Title:        s.Title,
		MessageCount: s.MessageCount,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
		Tokens:       s.PromptTokens + s.CompletionTokens,
		CostUSD:      s.Cost,
		YoloEnabled:  s.YoloEnabled,
	}
}

func deleteSessionByIDOrHash(ctx context.Context, a *app.App, idOrHash string) error {
	sess, err := resolveSessionID(ctx, a.Sessions, idOrHash)
	if err != nil {
		return err
	}
	if err := a.Sessions.Delete(ctx, sess.ID); err != nil {
		return fmt.Errorf("failed to delete session %s: %w", sess.ID, err)
	}
	fmt.Fprintf(os.Stderr, "deleted session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
	return nil
}

func short(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func sessionsShowCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	withMessages, _ := cmd.Flags().GetBool("with-messages")
	full, _ := cmd.Flags().GetBool("full")
	withSubagents, _ := cmd.Flags().GetBool("with-subagents")
	if full {
		withMessages = true
	}
	// --with-subagents renders child delegation transcripts, which only makes
	// sense alongside the parent's own message thread; imply --with-messages so
	// `show <id> --with-subagents` on its own does the obviously-intended thing.
	if withSubagents {
		withMessages = true
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	type msgItem struct {
		ID           string `json:"id"`
		Role         string `json:"role"`
		Preview      string `json:"preview"`
		FinishReason string `json:"finish_reason,omitempty"`
		// Retried mirrors printMessage's ndjson field: a finish_reason="error"
		// row that was followed by more messages was transiently retried, not
		// a terminal death. Separate boolean so consumers keep the raw enum.
		Retried bool `json:"retried,omitempty"`
	}

	type sessionShowOutput struct {
		ID               string  `json:"id"`
		Hash             string  `json:"hash"`
		Title            string  `json:"title"`
		Purpose          string  `json:"purpose,omitempty"` // first user prompt excerpt
		ParentID         string  `json:"parent_id,omitempty"`
		Provider         string  `json:"provider,omitempty"`
		Model            string  `json:"model,omitempty"`
		Effort           string  `json:"effort,omitempty"`
		CreatedAt        int64   `json:"created_at"`
		UpdatedAt        int64   `json:"updated_at"`
		MessageCount     int64   `json:"message_count"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		CostUSD          float64 `json:"cost_usd"`
		EndedReason      string  `json:"ended_reason,omitempty"`
		BudgetMaxCost    float64 `json:"budget_max_cost,omitempty"`
		BudgetMaxTokens  int64   `json:"budget_max_tokens,omitempty"`
		BudgetTimeoutSec int64   `json:"budget_timeout_sec,omitempty"`
		// SubAgentActivity, when non-empty, describes an in-flight sub-agent
		// delegation whose activity is fresher than this session's own —
		// e.g. "assistant activity 3s ago (session abc12345)". Computed from
		// the shared call-tree walk (sessions_activity.go).
		SubAgentActivity string    `json:"sub_agent_activity,omitempty"`
		SystemPrompt     string    `json:"system_prompt,omitempty"`
		Messages         []msgItem `json:"messages,omitempty"`
	}

	// Fetch the first user message as "purpose".
	var purpose string
	messages, msgErr := a.Messages.List(cmd.Context(), sess.ID)
	if msgErr == nil {
		for _, msg := range messages {
			if msg.Role == message.User {
				for _, part := range msg.Parts {
					if tc, ok := part.(message.TextContent); ok && tc.Text != "" {
						purpose = tc.Text
						if len(purpose) > 120 {
							purpose = purpose[:120] + "…"
						}
						break
					}
				}
				break
			}
		}
	}

	output := sessionShowOutput{
		ID:               sess.ID,
		Hash:             session.HashID(sess.ID),
		Title:            sess.Title,
		Purpose:          purpose,
		ParentID:         sess.ParentSessionID,
		Provider:         sess.LargeModelProvider,
		Model:            sess.LargeModelID,
		Effort:           sess.LargeModelReasoningEffort,
		CreatedAt:        sess.CreatedAt,
		UpdatedAt:        sess.UpdatedAt,
		MessageCount:     sess.MessageCount,
		PromptTokens:     sess.PromptTokens,
		CompletionTokens: sess.CompletionTokens,
		CostUSD:          sess.Cost,
		EndedReason:      sess.EndedReason,
		BudgetMaxCost:    sess.BudgetMaxCost,
		BudgetMaxTokens:  sess.BudgetMaxTokens,
		BudgetTimeoutSec: sess.BudgetTimeoutSec,
		SystemPrompt:     sess.SystemPrompt,
	}

	// Sub-agent pulse: surface an in-flight delegation's own last activity
	// so `sessions show` on a session that's blocked waiting on a sub-agent
	// isn't misread as idle. Baseline = the session's own updated_at; the
	// note only appears when a descendant sub-agent session is fresher.
	output.SubAgentActivity = subAgentActivityNote(cmd.Context(), a, sess.ID, sess.UpdatedAt, time.Now())

	if withMessages {
		if msgErr != nil {
			return fmt.Errorf("failed to list messages: %w", msgErr)
		}

		output.Messages = make([]msgItem, len(messages))
		for i, msg := range messages {
			preview := ""
			if full {
				for _, part := range msg.Parts {
					if tc, ok := part.(message.TextContent); ok {
						preview = tc.Text
						break
					}
				}
			} else {
				for _, part := range msg.Parts {
					if tc, ok := part.(message.TextContent); ok {
						preview = truncate(tc.Text, 200)
						break
					}
				}
			}

			finishReason := ""
			retried := false
			if f := msg.FinishPart(); f != nil {
				finishReason = string(f.Reason)
				// A followed-by-later error row is a transient, auto-retried
				// failure — the session went on. messages is the full list in
				// order, so any row but the last has a later message.
				retried = f.Reason == message.FinishReasonError && i < len(messages)-1
			}

			output.Messages[i] = msgItem{
				ID:           msg.ID,
				Role:         string(msg.Role),
				Preview:      preview,
				FinishReason: finishReason,
				Retried:      retried,
			}
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(output)
	}

	fmt.Printf("ID:           %s\n", output.ID)
	fmt.Printf("Hash:         %s\n", short(output.Hash))
	fmt.Printf("Title:        %s\n", output.Title)
	if output.ParentID != "" {
		fmt.Printf("Parent:       %s\n", output.ParentID)
	} else {
		fmt.Printf("Parent:       -\n")
	}
	if output.Provider != "" || output.Model != "" {
		fmt.Printf("Provider:     %s\n", output.Provider+"/"+output.Model)
		if output.Effort != "" {
			fmt.Printf("Effort:       %s\n", output.Effort)
		}
	}
	fmt.Printf("Created:      %s\n", time.Unix(output.CreatedAt, 0).Format(time.RFC3339))
	fmt.Printf("Updated:      %s\n", time.Unix(output.UpdatedAt, 0).Format(time.RFC3339))
	fmt.Printf("Messages:     %d\n", output.MessageCount)
	fmt.Printf("Tokens:       %d prompt, %d completion\n", output.PromptTokens, output.CompletionTokens)
	costLine := fmt.Sprintf("$%.6f USD", output.CostUSD)
	if output.BudgetMaxCost > 0 {
		pct := output.CostUSD / output.BudgetMaxCost * 100
		costLine += fmt.Sprintf(" / $%.2f budget (%.0f%%)", output.BudgetMaxCost, pct)
	}
	fmt.Printf("Cost:         %s\n", costLine)
	if output.BudgetMaxTokens > 0 {
		totalTokens := output.PromptTokens + output.CompletionTokens
		pct := float64(totalTokens) / float64(output.BudgetMaxTokens) * 100
		fmt.Printf("Token budget: %d / %d (%.0f%%)\n", totalTokens, output.BudgetMaxTokens, pct)
	}
	if output.BudgetTimeoutSec > 0 {
		fmt.Printf("Timeout:      %s\n", formatDurationShort(time.Duration(output.BudgetTimeoutSec)*time.Second))
	}
	if output.EndedReason != "" {
		fmt.Printf("Ended:        %s\n", output.EndedReason)
	}
	if output.SubAgentActivity != "" {
		fmt.Printf("Delegating:   %s\n", output.SubAgentActivity)
	}
	if output.Purpose != "" {
		fmt.Printf("Purpose:      %s\n", output.Purpose)
	}
	fmt.Println()
	fmt.Println("System prompt:")
	if output.SystemPrompt == "" {
		fmt.Println("  (none)")
	} else {
		lines := strings.Split(strings.TrimSpace(output.SystemPrompt), "\n")
		if len(lines) > 5 {
			for _, line := range lines[:5] {
				fmt.Printf("  %s\n", line)
			}
			fmt.Printf("  ... (%d more lines; use --with-messages for full)\n", len(lines)-5)
		} else {
			for _, line := range lines {
				fmt.Printf("  %s\n", line)
			}
		}
	}

	if output.Messages != nil {
		fmt.Println()
		fmt.Println("Messages:")
		for i, msg := range output.Messages {
			fmt.Printf("  %d. [%s] %s\n", i+1, msg.Role, truncate(msg.Preview, 60))
			if msg.FinishReason != "" {
				fmt.Printf("     (finished: %s)\n", finishReasonLabel(message.FinishReason(msg.FinishReason), msg.Retried))
			}
		}
	}

	// Opt-in (--with-subagents): after the parent's message summary, render
	// each sub-agent delegation's full transcript as a demarcated, indented
	// block. Default-hidden: without the flag, `show` never prints a child
	// session's message content (only the one-line pulse note above).
	if withSubagents {
		fmt.Println()
		fmt.Println("Sub-agent delegations:")
		printSubAgentTranscripts(cmd.Context(), os.Stdout, a, sess.ID, "text", time.Now())
	}

	return nil
}

// lockPulseStatus classifies a lock file by its heartbeat mtime.
// Heartbeat interval = 10s, stale threshold = 20s (session.lockStaleDuration).
//
//	0–10s  → "alive"    (fresh heartbeat)
//	10–15s → "ping"     (one beat overdue, likely OK)
//	15–20s → "stopping" (two beats missed, probably finishing)
//	>20s   → "offline"  (stale — holder crashed or exited without Release)
func lockPulseStatus(ageSec int64) string {
	switch {
	case ageSec <= 10:
		return "alive"
	case ageSec <= 15:
		return "ping"
	case ageSec <= 20:
		return "stopping"
	default:
		return "offline"
	}
}

// lockHolderProvablyDead decides, via a real OS-level lock attempt, whether
// the lock file at lockPath under dataDir/locks/session-<id>.lock may safely
// be auto-deleted. Mirrors sessions_kill.go's probeThenKillHolder — same
// "don't trust mtime alone, prove it via the real OS lock" discipline (task
// #222 hardening).
//
// Why mtime alone stopped being safe here: task #214/#222 gated the
// heartbeat's mtime-touch on real RecordActivity() calls instead of an
// unconditional 10s timer. A session blocked on a single long-running tool
// call (bounded by toolExecutionMaxDefault, up to 45 minutes) can now go
// well past the old 60s auto-delete threshold with zero recorded activity
// while still being completely healthy and still holding the real OS lock.
// Unconditionally os.Remove-ing the path in that case does NOT revoke the
// live holder's flock/LockFileEx (advisory locks are bound to the inode,
// not the path) — it just lets a SECOND process create a fresh inode at the
// same path and believe it owns the session, producing two simultaneous
// owners of one session id (see the package doc on session.SessionLock).
//
// Returns true only when TryAcquireSessionLock itself succeeds — i.e. we
// just proved, at the kernel level, that nobody holds the lock right now.
// The lock we acquired to prove that is released immediately. Any other
// outcome (busy error, or an unidentified failure) is treated as "do not
// delete" — the conservative default, since a false "provably dead" is what
// causes the two-owners bug, while a false "still alive" merely means the
// stale entry lingers one more `sessions locks` invocation.
//
// What this does NOT solve (task #230, narrower follow-ups to #222): two
// related gaps remain, both real but far narrower than the unconditional
// os.Remove this function replaced.
//
//  1. Residual TOCTOU between probe and removal. This function proves
//     "nobody holds the lock" and releases immediately; it does not itself
//     remove the file. Its caller (sessionsLocksCmdRun) calls os.Remove
//     afterward, as a separate, non-atomic step. In the gap between this
//     function's Release() and the caller's os.Remove(), a fresh
//     `crush run --session <id>` can legitimately re-acquire the same
//     session id and start writing — and the caller would then unlink
//     that new, live holder's lock file, reproducing (in a window on the
//     order of a syscall or two, not the original unbounded-mtime window)
//     the exact two-owners scenario this hardening exists to prevent.
//     Closing this fully would mean holding the OS lock across the
//     os.Remove itself (return the still-held *session.SessionLock to the
//     caller instead of releasing here, remove the path while holding it,
//     then release). That was deliberately NOT done: this package's sibling
//     session.FileLock.Release doc comment already documents why removing a
//     path while a lock on it is held is cross-platform-fragile — POSIX
//     unlink of a locked-but-open file is harmless (flock is keyed off the
//     inode), but the lock files here are opened without FILE_SHARE_DELETE
//     on Windows, so a concurrent opener's handle can make the delete fail
//     with a sharing violation, or a successful delete can race a
//     subsequent opener into creating a distinct file object at the same
//     path while an older locked one still exists — i.e. the fix risks
//     reintroducing a shaped version of the same two-owners bug on Windows
//     specifically, in exchange for closing a gap that is already down to
//     single-digit milliseconds. Not worth it here; the window is accepted
//     and documented instead.
//  2. Probe-induced transient contention. Proving death takes a real
//     exclusive OS lock (open + LockFileEx/flock + truncate + PID write +
//     Sync + sidecar write + Chtimes, then release) on every lock file
//     `sessions locks` inspects that looks older than autoDeleteAfter. A
//     `crush run` legitimately starting on that exact session id during
//     that brief window gets a hard "already in use" abort with no retry.
//     This is why the fix for (1) above is not simply "hold the lock
//     longer" — extending how long the probe holds the OS lock would widen
//     this exact contention window, trading one narrow risk for a wider
//     one. Given how narrow the window already is (a single acquire+release
//     cycle, not the run's full lifetime) and that it only collides with a
//     `crush run` starting on the SAME session id in that SAME instant,
//     this is accepted as-is rather than gated behind a flag — see
//     sessionsLocksCmdRun's doc comment for the fuller default-behavior
//     tradeoff.
func lockHolderProvablyDead(dataDir, sessionID string) bool {
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		// Busy (a real holder exists) or an unidentified probe failure —
		// neither is proof of death. Do not delete.
		return false
	}
	_ = lk.Release()
	return true
}

// preAutoDeleteRemoveHook is a test-only seam for sessionsLocksCmdRun's
// auto-delete branch. When non-nil, it is called synchronously between
// lockHolderProvablyDead proving a holder dead and the os.Remove call,
// allowing a test to deterministically reproduce the TOCTOU window where a
// concurrent process removes the lock file between the probe and the Remove.
// Always nil in production.
var preAutoDeleteRemoveHook func(lockPath string)

func sessionsLocksCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	staleOnly, _ := cmd.Flags().GetBool("stale-only")
	prune, _ := cmd.Flags().GetBool("prune")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	// Use the data directory setupApp already resolved onto `a` (honors
	// --data-dir and the project's configured data_directory) instead of
	// recomputing a cwd-based guess — see task #219/#224, and task #231
	// (this exact function was left unfixed when those landed).
	dataDir := a.Config().Options.DataDirectory
	locksDir := filepath.Join(dataDir, "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		if os.IsNotExist(err) {
			if asJSON {
				return nil
			}
			fmt.Println("(no locks)")
			return nil
		}
		return err
	}

	type lockItem struct {
		SessionID   string `json:"session_id"`
		PID         int    `json:"pid"`
		PulseSec    int64  `json:"pulse_sec"`
		Pulse       string `json:"pulse"` // alive / ping / stopping / offline
		AcquiredAt  int64  `json:"acquired_at_unix"`
		DurationSec int64  `json:"duration_seconds"`
		Stale       bool   `json:"stale"`
		BudgetSec   int64  `json:"budget_sec,omitempty"` // --timeout seconds, 0 if not set
		// SubAgent, when non-empty, means the freshest activity in this
		// session's call tree came from an in-flight sub-agent delegation,
		// NOT the top-level heartbeat — so PulseSec/Pulse below are the
		// sub-agent's activity age, which is the honest "is anything actually
		// making progress" signal an operator wants during a long delegation.
		SubAgent string `json:"sub_agent,omitempty"`
	}

	var locks []lockItem
	now := time.Now()
	const autoDeleteAfter = 60 * time.Second

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}

		sessionID := strings.TrimPrefix(entry.Name(), "session-")
		sessionID = strings.TrimSuffix(sessionID, ".lock")

		info, _ := entry.Info()
		age := now.Sub(info.ModTime())

		lockPath := filepath.Join(locksDir, entry.Name())

		// Auto-delete candidate: mtime older than 1 minute AND the operator
		// explicitly opted in via --prune. By default `sessions locks` is
		// READ-ONLY — it never removes anything. Deleting stable per-session
		// lock files is unnecessary in the first place (an empty lock file
		// with no held OS lock is harmless: the next acquirer reopens and
		// overwrites it, see internal/session/lock.go's Release), so the
		// whole class of TOCTOU "unlink a path that a live holder may be
		// reusing" bugs is closed by NOT auto-deleting unless an operator
		// asks for it.
		//
		// mtime alone is NO LONGER proof of death (task #222) — the
		// heartbeat's mtime touch is gated on RecordActivity, so a session
		// blocked on a single long-running tool call (up to
		// toolExecutionMaxDefault, 45 minutes) can look stale here while
		// still being completely healthy and still holding the real OS lock.
		// Before deleting under --prune, prove it via a real OS-level lock
		// attempt (lockHolderProvablyDead) — same discipline as
		// sessions_kill.go's probeThenKillHolder. Unlinking a path out from
		// under a live holder's flock/LockFileEx does not revoke it (advisory
		// locks are bound to the inode, not the path); it just lets a second
		// process create a fresh inode at the same path and believe it owns
		// the session — two owners of one session id.
		if prune && age > autoDeleteAfter {
			if lockHolderProvablyDead(dataDir, sessionID) {
				if preAutoDeleteRemoveHook != nil {
					preAutoDeleteRemoveHook(lockPath)
				}
				err := os.Remove(lockPath)
				switch {
				case err == nil, errors.Is(err, fs.ErrNotExist):
					// errors.Is(err, fs.ErrNotExist) (task #244): a concurrent
					// process — `sessions reap`, `sessions kill`, `sessions
					// reset --force`, or another parallel `sessions locks`
					// racing the same auto-delete path — may have already
					// removed this exact file between lockHolderProvablyDead's
					// probe above and this Remove call. The goal of this
					// Remove ("the stale lock file is gone from disk") is
					// already achieved in that case; it is not a failure.
					// Treating ENOENT as an error here (as task #234's fix
					// briefly did) produced a false "warning: could not
					// remove..." on stderr plus a phantom display-path row
					// below for a file that no longer exists (PID 0, offline,
					// age computed from the stale cached entry.Info()) —
					// exactly the false positive an orchestrating script
					// polling `sessions locks` alongside a concurrent
					// `sessions kill` would hit. Report success and move on,
					// same as the pre-#234 behavior for this specific case.
					fmt.Fprintf(os.Stderr, "removed stale lock %s (age %ds, holder provably dead)\n", entry.Name(), int(age.Seconds()))
					continue
				default:
					// Do NOT silently swallow this (task #234). A failed
					// Remove here is not a harmless no-op: lockHolderProvablyDead's
					// own acquire+release probe (see its doc comment) truncates
					// the lock file's content and freshens its mtime as part of
					// proving death, moments before this Remove runs. If the
					// Remove then fails for a REAL reason (e.g. a Windows
					// sharing-violation window from a concurrent opener that
					// still holds the file open, or permission denied — NOT
					// the file already being gone, which is handled above),
					// the file is left behind with a wiped PID AND a fresh
					// mtime — which every PID-fallback/mtime-based liveness
					// consumer below (and in isSessionLockAlive /
					// InspectSessionLock / computeSessionStatuses) would read
					// as LIVE for the next heartbeat-stale window, even though
					// this probe JUST proved the holder dead. Silently
					// `continue`-ing also made the entry vanish from this
					// listing entirely, so an operator had zero signal
					// anything was wrong. Surface the failure, then fall
					// through to the normal display path below instead of
					// `continue`-ing: age is already > autoDeleteAfter (60s),
					// which lockPulseStatus always classifies as "offline"
					// (>20s), so the entry is correctly shown as stale/offline
					// rather than silently dropped from the listing while its
					// stale file lingers on disk.
					fmt.Fprintf(os.Stderr, "warning: could not remove provably-dead lock %s: %v\n", entry.Name(), err)
				}
			}
			// mtime looks stale but the real OS lock is still held (or the
			// probe was inconclusive), or the provably-dead lock's removal
			// itself failed — do NOT delete (or it's already gone from disk
			// in the success case above, which already `continue`d). Fall
			// through and display it like any other lock; its Pulse will
			// read "offline" so operators still see it's not heartbeating,
			// without risking a second process reclaiming a session that is
			// still alive.
		}

		pulseSec := int64(age.Seconds())
		pulse := lockPulseStatus(pulseSec)
		stale := pulse == "offline"

		// Call-tree pulse override: the lock mtime only ticks on real
		// activity the heartbeat goroutine's RecordActivity gate observed
		// (stream chunks — task #300), NOT on tool-call execution itself.
		// A session can be genuinely busy running ordinary top-level tool
		// calls (view, todos, ...) between model responses, with no stream
		// chunk arriving for well over the heartbeat's stale threshold —
		// the heartbeat mtime then reads "offline" for a live, working
		// session (task #321; observed live: PULSE_AGE == ELAPSED == 36s
		// with the process alive and real tool calls in progress).
		// computeCallTreeActivity's LatestUnix already covers the ROOT
		// session's own message activity (created_at/updated_at, bumped on
		// every tool-input-start/tool-call/tool-result), not just a
		// descendant sub-agent's — so this override no longer requires
		// act.SubAgentActive to apply. It's still used below to decide
		// whether to show a specific sub-agent's hash: a fresher signal
		// that came from the session's OWN activity, not a delegation,
		// gets no such label.
		var subAgentLabel string
		if act, fresher := callTreeActivityFresherThan(cmd.Context(), a, sessionID, info.ModTime().Unix()); fresher {
			if subAge, ok := act.Age(now); ok {
				pulseSec = int64(subAge.Seconds())
				pulse = lockPulseStatus(pulseSec)
				stale = pulse == "offline"
				if act.SubAgentActive {
					subAgentLabel = short(session.HashID(act.DeepestSessionID))
				}
			}
		}

		if staleOnly && !stale {
			continue
		}

		// session.ReadLockPID (not a raw os.ReadFile+Sscanf) so a live
		// holder's PID is still readable on Windows, where the holder's
		// mandatory LockFileEx range-lock can make the lock file's own
		// content unreadable to this process — see readLockFile's sidecar
		// fallback (internal/session/lock.go) and task #231.
		pid := session.ReadLockPID(lockPath)
		budgetSec := session.ReadLockTimeoutSec(lockPath)

		// Approximate acquire time: mtime when pulse was fresh.
		// For alive locks mtime ≈ now, so we use file birthtime via stat
		// if available; otherwise mtime is the best proxy.
		acqTime := info.ModTime().Unix()
		duration := int64(now.Sub(info.ModTime()).Seconds())

		locks = append(locks, lockItem{
			SessionID:   sessionID,
			PID:         pid,
			PulseSec:    pulseSec,
			Pulse:       pulse,
			AcquiredAt:  acqTime,
			DurationSec: duration,
			Stale:       stale,
			BudgetSec:   budgetSec,
			SubAgent:    subAgentLabel,
		})
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, lock := range locks {
			if err := enc.Encode(lock); err != nil {
				return err
			}
		}
		return nil
	}

	if len(locks) == 0 {
		if staleOnly {
			fmt.Println("(no stale locks)")
		} else {
			fmt.Println("(no locks)")
		}
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION_ID\tPID\tPULSE\tPULSE_AGE\tELAPSED\tBUDGET\tSUB-AGENT")
	for _, lock := range locks {
		budget := "∞"
		if lock.BudgetSec > 0 {
			budget = formatDurationShort(time.Duration(lock.BudgetSec) * time.Second)
		}
		subAgent := "-"
		if lock.SubAgent != "" {
			subAgent = lock.SubAgent
		}
		fmt.Fprintf(
			tw, "%s\t%d\t%s\t%ds ago\t%s\t%s\t%s\n",
			truncate(lock.SessionID, 28),
			lock.PID,
			lock.Pulse,
			lock.PulseSec,
			formatDurationShort(time.Duration(lock.DurationSec)*time.Second),
			budget,
			subAgent,
		)
	}
	return tw.Flush()
}

func sessionsTailCmdRun(cmd *cobra.Command, args []string) error {
	follow, _ := cmd.Flags().GetBool("follow")
	fromMsgID, _ := cmd.Flags().GetString("from-message")
	format, _ := cmd.Flags().GetString("format")
	withSubagents, _ := cmd.Flags().GetBool("with-subagents")

	if format != "text" && format != "ndjson" {
		return fmt.Errorf("invalid format: %s (must be text or ndjson)", format)
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sessionID := args[0]
	// Verify session exists
	_, err = resolveSessionID(cmd.Context(), a.Sessions, sessionID)
	if err != nil {
		return err
	}

	// Track the last message ID we've printed
	lastPrinted := fromMsgID

	// Print existing messages
	messages, err := a.Messages.List(cmd.Context(), sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	// Filter by fromMsgID if set
	if fromMsgID != "" {
		found := false
		for i, msg := range messages {
			if msg.ID == fromMsgID {
				messages = messages[i+1:]
				found = true
				break
			}
		}
		if found {
			lastPrinted = fromMsgID
		}
	}

	// Build origin context from the snapshot we have right now; the
	// follow loop below extends it as new ToolCall parts arrive.
	callCtx := buildToolCallContext(messages)

	// Print messages. This batch ends at the session tail, so a row is
	// "followed by a later message" iff it isn't the last in the slice —
	// which is how finishReasonLabel distinguishes an auto-retried error
	// from a terminal one.
	now := time.Now()
	for i, msg := range messages {
		printMessageWithTime(os.Stdout, msg, format, now, callCtx, i < len(messages)-1)
		lastPrinted = msg.ID
	}

	// Opt-in (--with-subagents): render each sub-agent delegation's transcript
	// as a demarcated block after the parent stream. Rendered once (for the
	// snapshot at this point) rather than re-emitted on every follow tick, so
	// --follow doesn't repeat the whole child transcript each second.
	if withSubagents {
		printSubAgentTranscripts(cmd.Context(), os.Stdout, a, sessionID, format, now)
	}

	if !follow {
		return nil
	}

	// Check if session is already finished
	isFinished := func() bool {
		msgs, err := a.Messages.List(cmd.Context(), sessionID)
		if err != nil || len(msgs) == 0 {
			return false
		}
		lastMsg := msgs[len(msgs)-1]
		if f := lastMsg.FinishPart(); f != nil && !f.Partial {
			return true
		}
		return false
	}

	// Poll for new messages
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		messages, err := a.Messages.List(cmd.Context(), sessionID)
		if err != nil {
			return fmt.Errorf("database error: %w", err)
		}

		// Rebuild origin context — new ToolCall parts may have arrived
		// this tick, and the next ToolResult render needs them.
		callCtx = buildToolCallContext(messages)

		// Print any new messages. lastIdx, not a CreatedAt/ID comparison:
		// messages is already the DB's own deterministic total order
		// (ListMessagesBySession's `ORDER BY created_at ASC, rowid ASC` —
		// created_at alone has only one-second granularity, so several
		// messages from one fast turn routinely tie). Re-deriving "after"
		// from CreatedAt+ID (the old isAfter) discarded that order and
		// substituted a coinflip on message.ID, a random UUID, whenever two
		// messages landed in the same second (task #319). Trusting the
		// slice's own position is both simpler and correct by construction.
		now := time.Now()
		lastIdx := indexByID(messages, lastPrinted)
		for i := range messages {
			if i > lastIdx {
				printMessageWithTime(os.Stdout, messages[i], format, now, callCtx, i < len(messages)-1)
				lastPrinted = messages[i].ID
			}
		}

		// Check if finished
		if isFinished() {
			return nil
		}
	}

	return nil
}

// indexByID returns the index of the message with the given id in
// messages, or -1 if id is empty or not found (including the case where a
// previously-printed message no longer appears in a fresh List() result —
// matching the old findByID+isAfter behavior of treating a missing anchor
// as "print everything").
func indexByID(messages []message.Message, id string) int {
	if id == "" {
		return -1
	}
	for i := range messages {
		if messages[i].ID == id {
			return i
		}
	}
	return -1
}

// formatAgo returns a human-friendly "X ago" string for the given duration.
func formatAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		return fmt.Sprintf("%dd %dh ago", days, hours)
	}
}

// formatDurationShort returns a compact "Xm Ys" or "Xh Ym" string.
func formatDurationShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// toolCallOrigin captures the (name, raw JSON input) of an assistant
// ToolCall, indexed by its ToolCallID so that the subsequent
// ToolResult render can pull out the call's most useful argument
// (file_path, url, pattern, etc.) and show it next to the result.
// Populated by buildToolCallContext before a batch render.
type toolCallOrigin struct {
	name  string
	input string
}

// buildToolCallContext walks every message in a session and indexes every
// ToolCall by its ID. The map is then handed to printMessageWithTime so
// renderings of ToolResult parts can look up "what was the call about"
// and prefix the result preview with the argument (e.g. file_path for
// view, url for fetch). Walking is O(N+M) over messages and parts;
// caller pays this once per render batch.
func buildToolCallContext(msgs []message.Message) map[string]toolCallOrigin {
	out := make(map[string]toolCallOrigin, len(msgs))
	for _, m := range msgs {
		for _, part := range m.Parts {
			tc, ok := part.(message.ToolCall)
			if !ok || tc.ID == "" {
				continue
			}
			out[tc.ID] = toolCallOrigin{name: tc.Name, input: tc.Input}
		}
	}
	return out
}

// lookupToolCallOrigin returns the (name, input) recorded for toolCallID,
// or ("", "") when the context is nil or the id is unknown. Safe to call
// with a nil map — callers that don't need origin enrichment (legacy
// paths) can pass nil and get the old behaviour from
// formatToolResultPreview.
func lookupToolCallOrigin(ctx map[string]toolCallOrigin, toolCallID string) (string, string) {
	if ctx == nil {
		return "", ""
	}
	o, ok := ctx[toolCallID]
	if !ok {
		return "", ""
	}
	return o.name, o.input
}

// printMessageWithTime prints a timestamp header followed by the message
// content. Only adds the header in text format when CreatedAt != 0.
// A blank line is printed between messages for readability. callCtx
// (optional, may be nil) maps ToolCallID to the originating ToolCall's
// name and JSON input — when present, ToolResult rendering uses it to
// show the call's most useful argument next to the result.
//
// followedByLater reports whether a newer message exists in the same
// session after this one. It only affects how a FinishReasonError row is
// labelled: a bare "(finished: error)" reads like the process died there,
// but if the session went on to produce more rows the error was transient
// and the turn was re-run (coordinator transient-retry, an orchestrator
// re-invocation, or a Phase-4 auto-resume) — see finishReasonLabel.
func printMessageWithTime(w io.Writer, msg message.Message, format string, now time.Time, callCtx map[string]toolCallOrigin, followedByLater bool) {
	if format == "text" && msg.CreatedAt != 0 {
		ts := time.Unix(msg.CreatedAt, 0)
		ago := now.Sub(ts)
		fmt.Fprintf(w, "[%s] (%s)\n", ts.Format("2006-01-02 15:04:05"), formatAgo(ago))
	}
	printMessage(w, msg, format, callCtx, followedByLater)
}

// finishReasonLabel renders the "(finished: …)" suffix for a message row.
// For a FinishReasonError that is NOT the session's final row, it appends a
// note that the turn was retried — the same underlying finish_reason="error"
// means "the process died here" only when nothing came after it. Without
// this, a transient, auto-retried failure is indistinguishable from a
// terminal one in `sessions last` / `sessions show`.
func finishReasonLabel(reason message.FinishReason, followedByLater bool) string {
	if reason == message.FinishReasonError && followedByLater {
		return string(reason) + " — retried, session continued"
	}
	return string(reason)
}

func printMessage(w io.Writer, msg message.Message, format string, callCtx map[string]toolCallOrigin, followedByLater bool) {
	if format == "ndjson" {
		type msgJSON struct {
			ID           string `json:"id"`
			Role         string `json:"role"`
			Preview      string `json:"preview"`
			FinishReason string `json:"finish_reason,omitempty"`
			// Retried is true for a finish_reason="error" row that was
			// followed by more messages in the session — i.e. the error was
			// transient and the turn was re-run, not a terminal death. Kept
			// as a separate boolean so consumers can still switch on the raw
			// finish_reason enum. Omitted (false) for every non-error row.
			Retried bool `json:"retried,omitempty"`
		}
		preview := ""
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok {
				preview = truncate(tc.Text, 200)
				break
			}
		}
		finishReason := ""
		retried := false
		if f := msg.FinishPart(); f != nil {
			finishReason = string(f.Reason)
			retried = f.Reason == message.FinishReasonError && followedByLater
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(msgJSON{
			ID:           msg.ID,
			Role:         string(msg.Role),
			Preview:      preview,
			FinishReason: finishReason,
			Retried:      retried,
		})
	} else {
		// text format
		fmt.Fprintf(w, "[%s]\n", msg.Role)
		rendered := 0
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case message.TextContent:
				if p.Text == "" {
					continue
				}
				fmt.Fprintf(w, "%s\n", p.Text)
				rendered++
			case message.ReasoningContent:
				if p.Thinking == "" {
					continue
				}
				fmt.Fprintf(w, "[thinking] %s\n", truncatePreview(firstLine(p.Thinking), 200))
				rendered++
			case message.ToolCall:
				if preview := formatToolCallPreview(p.Name, p.Input); preview != "" {
					fmt.Fprintf(w, "[tool: %s] %s\n", p.Name, preview)
				} else {
					fmt.Fprintf(w, "[tool: %s]\n", p.Name)
				}
				rendered++
			case message.ToolResult:
				name := p.Name
				if name == "" {
					name = p.ToolCallID
				}
				originName, originInput := lookupToolCallOrigin(callCtx, p.ToolCallID)
				preview := formatToolResultPreview(p.Content, originName, originInput)
				prefix := "[tool-result: " + name + "]"
				if p.IsError {
					prefix += " ERROR"
				}
				if preview != "" {
					fmt.Fprintf(w, "%s %s\n", prefix, preview)
				} else {
					fmt.Fprintf(w, "%s\n", prefix)
				}
				rendered++
			}
		}
		if rendered == 0 {
			// No renderable parts yet — most often a streaming row that
			// hasn't flushed text, or an auto-checkpoint placeholder with
			// only a partial Finish. Saying so explicitly is friendlier
			// than leaving a bare role header.
			fmt.Fprintf(w, "(no content yet)\n")
		}
		if f := msg.FinishPart(); f != nil && f.Reason != "" {
			fmt.Fprintf(w, "(finished: %s)\n", finishReasonLabel(f.Reason, followedByLater))
		}
		fmt.Fprintf(w, "\n")
	}
}
