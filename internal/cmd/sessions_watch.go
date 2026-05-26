package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsWatchCmd = &cobra.Command{
	Use:   "watch [session-id]",
	Short: "Pick a session (or take one by id) and live-tail it until it ends",
	Long: `One-stop "open a live view of a session" command.

Without arguments: shows an interactive picker (arrow keys, Enter to
select) and then drops into live-tail of the chosen session.

With a <session-id> argument: skips the picker and live-tails that
session directly. Short hashes (the HASH column of "sessions list")
are accepted.

Live-tail prints every existing message in the session, then polls
every --interval (default 1s) for new messages and prints them as they
arrive. The loop exits automatically when the session ends — detected
via any of:
  (a) the session row has an ended_reason
  (b) the lock file disappears (process exited / was killed)
  (c) the latest assistant message has a non-partial Finish part

On exit a summary block is printed: id, title, end reason, duration,
tokens (prompt + completion) and cost (with budget if one was set).

Ctrl+C interrupts and prints "(interrupted — session still running)"
without a summary so you don't mistake "I stopped watching" for
"the session ended".`,
	Example: `
# Pick a session interactively and live-tail it
crush sessions watch

# Live-tail a specific session (full id or short hash)
crush sessions watch abc123

# Faster polling for snappier output
crush sessions watch --interval 500ms
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: sessionsWatchCmdRun,
}

func sessionsWatchCmdRun(cmd *cobra.Command, args []string) error {
	interval, _ := cmd.Flags().GetDuration("interval")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	locksDir := filepath.Join(cwd, ".crush", "locks")
	ctx := cmd.Context()

	var sessionID string
	if len(args) == 1 {
		sess, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}
		sessionID = sess.ID
	} else {
		picked, err := pickSessionForWatch(ctx, a)
		if err != nil {
			return err
		}
		if picked == "" {
			return nil
		}
		sessionID = picked
	}

	return liveTailSession(ctx, a, sessionID, locksDir, interval)
}

func init() {
	sessionsWatchCmd.Flags().Duration("interval", time.Second, "Poll interval for new messages (e.g. 1s, 500ms, 2s)")
}

// liveTailSession prints every existing message in a session and then
// polls for new messages until the session ends. See the command Long
// description for the end-detection signals. On exit it prints a final
// summary block (duration, cost, tokens, ended_reason).
func liveTailSession(ctx context.Context, a *app.App, sessionID, locksDir string, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}

	sess, err := a.Sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session %s: %w", sessionID, err)
	}

	fmt.Fprintf(os.Stderr, "watching session %s (%s)\n", truncate(sess.ID, 12), truncate(sess.Title, 60))
	fmt.Fprintln(os.Stderr, "(Ctrl+C to exit early)")
	fmt.Fprintln(os.Stderr)

	messages, err := a.Messages.List(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	// Origin context lets ToolResult renderings show the file_path /
	// url / pattern the call was about. Rebuilt every tick because new
	// ToolCalls arrive over the polling loop.
	callCtx := buildToolCallContext(messages)

	now := time.Now()
	lastPrinted := ""
	for _, msg := range messages {
		printMessageWithTime(os.Stdout, msg, "text", now, callCtx)
		lastPrinted = msg.ID
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// Check for end first so we print a summary even when there are
		// no new messages to emit on this tick.
		if done, reason := isSessionFinished(ctx, a, sessionID, locksDir); done {
			printWatchSummary(os.Stderr, ctx, a, sessionID, reason)
			return nil
		}

		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "\n(interrupted — session still running)")
			return nil
		case <-ticker.C:
		}

		msgs, err := a.Messages.List(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("database error: %w", err)
		}
		callCtx = buildToolCallContext(msgs)
		tickNow := time.Now()
		var anchor *message.Message
		if lastPrinted != "" {
			anchor = findByID(msgs, lastPrinted)
		}
		for i := range msgs {
			if msgs[i].ID == lastPrinted {
				continue
			}
			if lastPrinted == "" || isAfter(&msgs[i], anchor) {
				printMessageWithTime(os.Stdout, msgs[i], "text", tickNow, callCtx)
				lastPrinted = msgs[i].ID
			}
		}
	}
}

// liveLockMaxAge is the threshold for considering a lock file "alive".
// The session heartbeat touches the lock every 10s; we add a 10s grace
// window so a missed tick during a slow GC pause / disk sync does not
// look like a dead process. Matches session.lockStaleDuration in spirit.
const liveLockMaxAge = 20 * time.Second

// isSessionFinished reports whether a live-tail loop should exit. Returns
// the end reason as a short human label so the summary block can show
// it next to "reason:". I/O-doing wrapper; the pure decision lives in
// isSessionFinishedFromState so it is unit-testable without an app /
// filesystem.
func isSessionFinished(ctx context.Context, a *app.App, sessionID, locksDir string) (bool, string) {
	sess, sessErr := a.Sessions.Get(ctx, sessionID)
	msgs, msgsErr := a.Messages.List(ctx, sessionID)
	lockPath := filepath.Join(locksDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")

	// Distinguish "alive" (file exists and was touched recently — process
	// is still heartbeating) from "stale or missing" (file gone, or file
	// present but mtime older than the heartbeat window — holder crashed
	// or detached). Only "alive" should block the end signals.
	var lockAlive bool
	if info, err := os.Stat(lockPath); err == nil {
		lockAlive = time.Since(info.ModTime()) < liveLockMaxAge
	}
	return isSessionFinishedFromState(sess, sessErr, msgs, msgsErr, lockAlive)
}

// isSessionFinishedFromState is the pure decision used by isSessionFinished.
//
// The lock heartbeat is the AUTHORITATIVE signal: while the holding
// process is alive (lock mtime < liveLockMaxAge) we never terminate the
// watch, regardless of what the DB rows say. This guards against the
// real-world failure mode where a tool-result message carries a Finish
// part with reason="stop" (the tool finished — not the session), or
// where an assistant message has Finish reason="tool_use" (it ran a
// tool and is about to consume the result, not done).
//
// Only when the lock is no longer alive do we trust the DB-derived
// signals:
//
//	(a) session row has a non-empty EndedReason
//	(b) lock disappeared / went stale AND the session has at least one
//	    message (the "at least one message" guard avoids racing the
//	    acquirer that has opened the file but not yet touched / written
//	    the lock)
//	(c) the latest ASSISTANT message has a non-partial Finish whose
//	    Reason is a terminal FinishReason (end_turn / max_tokens /
//	    canceled / error). tool_use, unknown, and any unrecognised
//	    string are treated as "not yet done" — the agent is mid-loop.
//
// Errors on the session lookup are treated as "no signal (a)", and
// errors on the message lookup as "no signal (b)/(c)" — neither is
// treated as termination, so a transient DB hiccup does not end the tail.
func isSessionFinishedFromState(
	sess session.Session,
	sessErr error,
	msgs []message.Message,
	msgsErr error,
	lockAlive bool,
) (bool, string) {
	if lockAlive {
		return false, ""
	}
	if sessErr == nil && sess.EndedReason != "" {
		return true, sess.EndedReason
	}
	if msgsErr == nil && len(msgs) > 0 {
		// Walk back to the latest assistant message — tool result rows
		// carry their own Finish parts (e.g. reason="stop" when the
		// tool subprocess exits) that have nothing to do with end of
		// session. Only the assistant author's own finish counts.
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.Role != message.Assistant {
				continue
			}
			f := m.FinishPart()
			if f == nil || f.Partial {
				break
			}
			if isTerminalFinishReason(f.Reason) {
				return true, string(f.Reason)
			}
			// Latest assistant has Finish but it's tool_use / unknown /
			// some unrecognised reason — the loop is mid-step, not done.
			break
		}
		// Lock is not alive AND we have at least one message — the
		// holder process is gone but the session never wrote an
		// EndedReason or a terminal assistant Finish. Treat as ended.
		return true, "lock_released"
	}
	return false, ""
}

// isTerminalFinishReason reports whether a FinishReason indicates the
// agent has finished its work for this turn AND has nothing queued
// (i.e. it is safe to consider the session done). tool_use means the
// agent ran a tool and will continue after the result; unknown means
// we cannot tell; everything else recognised is a real end.
func isTerminalFinishReason(r message.FinishReason) bool {
	switch r {
	case message.FinishReasonEndTurn,
		message.FinishReasonMaxTokens,
		message.FinishReasonCanceled,
		message.FinishReasonError:
		return true
	}
	return false
}

// printWatchSummary emits the final block shown when a watched session
// finishes. Pulls fresh totals from the session row so any in-flight
// IncrementCost from the agent's last step is reflected. Thin wrapper;
// the formatting lives in formatWatchSummary so it can be unit-tested
// without a live app.
func printWatchSummary(w io.Writer, ctx context.Context, a *app.App, sessionID, reason string) {
	sess, err := a.Sessions.Get(ctx, sessionID)
	if err != nil {
		fmt.Fprintf(w, "\n--- session ended (could not load summary: %v) ---\n", err)
		return
	}
	fmt.Fprint(w, formatWatchSummary(sess, reason, time.Now()))
}

// formatWatchSummary renders the human-readable end-of-watch block.
// "now" is taken as an argument so tests can pin duration to a known
// value without sleeping. Layout (one blank line above for separation
// from the live message stream):
//
//	--- session ended ---
//	id:       <session-id>
//	title:    <title>           (omitted when empty)
//	reason:   <reason>
//	duration: <X>h<Y>m / <Y>m<Z>s / <Z>s  (compact form)
//	tokens:   <total> (prompt <p> + completion <c>)
//	cost:     $0.0000 [ / $X.XXXX budget ]
func formatWatchSummary(sess session.Session, reason string, now time.Time) string {
	duration := time.Duration(0)
	if sess.CreatedAt > 0 {
		duration = now.Sub(time.Unix(sess.CreatedAt, 0))
	}
	tokens := sess.PromptTokens + sess.CompletionTokens
	var b strings.Builder
	b.WriteString("\n--- session ended ---\n")
	fmt.Fprintf(&b, "id:       %s\n", sess.ID)
	if sess.Title != "" {
		fmt.Fprintf(&b, "title:    %s\n", sess.Title)
	}
	fmt.Fprintf(&b, "reason:   %s\n", reason)
	fmt.Fprintf(&b, "duration: %s\n", formatDurationShort(duration))
	fmt.Fprintf(&b, "tokens:   %s (prompt %s + completion %s)\n",
		formatWatchInt(tokens), formatWatchInt(sess.PromptTokens), formatWatchInt(sess.CompletionTokens))
	fmt.Fprintf(&b, "cost:     $%.4f", sess.Cost)
	if sess.BudgetMaxCost > 0 {
		fmt.Fprintf(&b, " / $%.4f budget", sess.BudgetMaxCost)
	}
	b.WriteString("\n")
	return b.String()
}

// pickSessionForWatch runs the interactive picker used by both
// "sessions pick" and "sessions watch". Returns "" when the user quits
// without selecting.
func pickSessionForWatch(ctx context.Context, a *app.App) (string, error) {
	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	// Filter out internal child sessions — same convention as sessions pick.
	visible := sessions[:0]
	for _, s := range sessions {
		if s.ParentSessionID != "" {
			continue
		}
		visible = append(visible, s)
	}
	if len(visible) == 0 {
		fmt.Fprintln(os.Stderr, "(no sessions)")
		return "", nil
	}

	items := make([]sessionItem, len(visible))
	now := time.Now()
	for i, s := range visible {
		items[i] = sessionItem{
			id:      s.ID,
			hash:    short(session.HashID(s.ID)),
			title:   truncate(s.Title, 40),
			updated: time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04"),
			cost:    s.Cost,
			ago:     formatAge(now.Sub(time.Unix(s.UpdatedAt, 0))),
		}
	}

	m := pickerModel{
		items:  items,
		cursor: 0,
		binary: os.Args[0],
	}
	p := tea.NewProgram(&m)
	if _, err := p.Run(); err != nil {
		return "", fmt.Errorf("failed to run picker: %w", err)
	}
	if m.quit || m.selected == "" {
		return "", nil
	}
	return m.selected, nil
}

// formatWatchInt thousands-separates a token count for the summary line.
// (Renamed from the old dashboard helper so it doesn't read like the
// removed feature was still around.)
func formatWatchInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// formatAge formats a duration for the picker's "ago" column. Used by
// both sessions_pick.go and sessions_watch.go.
func formatAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}
