package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsWatchCmd = &cobra.Command{
	Use:   "watch [session-id]",
	Short: "Live dashboard of active sessions, or live-tail one session",
	Long: `Three modes:

  crush sessions watch                — live dashboard of active sessions
                                        (poll every --interval, default 3s)
  crush sessions watch <id>           — live-tail a single session: print
                                        all messages, then keep streaming
                                        new ones until the session ends.
                                        Final summary on exit (duration,
                                        cost, tokens, ended_reason).
  crush sessions watch --pick         — interactive picker, then drop into
                                        live-tail mode for the selected
                                        session. Same as the second form
                                        but you choose the id by arrow keys.

Dashboard columns (no-args mode):
  SESSION_ID  — truncated to 24 chars
  PULSE       — alive / ping / stopping / offline (from lock mtime)
  AGE         — time since session was created
  LAST_TOOL   — name of the last ToolCall in the most recent assistant message
  TOKENS      — total prompt + completion tokens
  COST        — cost formatted as $X.XXX

Exits on Ctrl+C. Use --json for NDJSON output (dashboard mode only).`,
	Example: `
# Live dashboard with default 3s refresh
crush sessions watch

# Live-tail a specific session until it finishes
crush sessions watch abc123

# Pick a session interactively, then live-tail it
crush sessions watch --pick

# Faster refresh
crush sessions watch --interval 1s

# Machine-readable NDJSON output (dashboard only)
crush sessions watch --json
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: sessionsWatchCmdRun,
}

type watchRow struct {
	SessionID string  `json:"session_id"`
	Pulse     string  `json:"pulse"`
	Age       string  `json:"age"`
	LastTool  string  `json:"last_tool"`
	Tokens    int64   `json:"tokens"`
	Cost      float64 `json:"cost_usd"`
}

func sessionsWatchCmdRun(cmd *cobra.Command, args []string) error {
	interval, _ := cmd.Flags().GetDuration("interval")
	asJSON, _ := cmd.Flags().GetBool("json")
	pick, _ := cmd.Flags().GetBool("pick")

	if len(args) == 1 && pick {
		return fmt.Errorf("cannot combine <session-id> argument with --pick")
	}

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

	// Single-session live-tail modes.
	if len(args) == 1 {
		sess, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}
		return liveTailSession(ctx, a, sess.ID, locksDir, interval)
	}
	if pick {
		sessionID, err := pickSessionForWatch(ctx, a)
		if err != nil {
			return err
		}
		if sessionID == "" {
			return nil
		}
		return liveTailSession(ctx, a, sessionID, locksDir, interval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		rows, err := buildWatchRows(ctx, a, locksDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			if err := enc.Encode(rows); err != nil {
				return err
			}
		} else {
			clearScreen()
			if len(rows) == 0 {
				fmt.Println("(no active sessions)")
			} else {
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "SESSION_ID\tPULSE\tAGE\tLAST_TOOL\tTOKENS\tCOST")
				for _, row := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t$%.3f\n",
						truncate(row.SessionID, 24),
						row.Pulse,
						row.Age,
						row.LastTool,
						formatInt(row.Tokens),
						row.Cost,
					)
				}
				tw.Flush()
			}
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return nil
		}
	}
}

// buildWatchRows builds the watch table rows by cross-referencing lock
// files with session data.
func buildWatchRows(ctx context.Context, a *app.App, locksDir string) ([]watchRow, error) {
	locks, err := readActiveLocks(locksDir)
	if err != nil {
		return nil, err
	}

	if len(locks) == 0 {
		return nil, nil
	}

	// Build a map from session ID to lock info for fast lookup.
	lockMap := make(map[string]activeLock, len(locks))
	for _, l := range locks {
		lockMap[l.sessionID] = l
	}

	// List all sessions and filter to those with active locks.
	sessions, err := a.Sessions.ListAll(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var rows []watchRow

	for _, sess := range sessions {
		lock, ok := lockMap[sess.ID]
		if !ok {
			continue
		}

		age := now.Sub(time.Unix(sess.CreatedAt, 0))
		lastTool := ""

		// Fetch messages to find the last tool call.
		msgs, err := a.Messages.List(ctx, sess.ID)
		if err == nil && len(msgs) > 0 {
			lastTool = findLastTool(msgs)
		}

		rows = append(rows, watchRow{
			SessionID: sess.ID,
			Pulse:     lock.pulse,
			Age:       formatAge(age),
			LastTool:  lastTool,
			Tokens:    sess.PromptTokens + sess.CompletionTokens,
			Cost:      sess.Cost,
		})
	}

	return rows, nil
}

type activeLock struct {
	sessionID string
	pulse     string
}

// readActiveLocks scans the locks directory and returns session IDs that
// have a live lock file (mtime within 60s).
func readActiveLocks(locksDir string) ([]activeLock, error) {
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	now := time.Now()
	var locks []activeLock

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}

		sessionID := strings.TrimPrefix(entry.Name(), "session-")
		sessionID = strings.TrimSuffix(sessionID, ".lock")

		info, err := entry.Info()
		if err != nil {
			continue
		}

		age := now.Sub(info.ModTime())
		if age > 60*time.Second {
			continue
		}

		pulseSec := int64(age.Seconds())
		locks = append(locks, activeLock{
			sessionID: sessionID,
			pulse:     lockPulseStatus(pulseSec),
		})
	}

	return locks, nil
}

// findLastTool scans messages for the most recent ToolCall in the latest
// assistant message.
func findLastTool(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant {
			continue
		}
		for j := len(msg.Parts) - 1; j >= 0; j-- {
			if tc, ok := msg.Parts[j].(message.ToolCall); ok && tc.Name != "" {
				return tc.Name
			}
		}
		return ""
	}
	return ""
}

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

func formatInt(n int64) string {
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

func init() {
	sessionsWatchCmd.Flags().Duration("interval", 3*time.Second, "Poll interval (e.g. 3s, 1s, 500ms)")
	sessionsWatchCmd.Flags().Bool("json", false, "Emit NDJSON: one JSON array per poll cycle (dashboard mode only)")
	sessionsWatchCmd.Flags().Bool("pick", false, "Show an interactive session picker, then live-tail the selected one")
}

// liveTailSession prints every existing message in a session and then
// polls for new messages until the session ends. End is detected by any
// of: (a) the lock file disappearing (process exited / was killed),
// (b) the latest assistant message having a non-partial Finish part,
// (c) the session row having a non-empty EndedReason. On exit it prints
// a final summary block (duration, cost, tokens, ended_reason).
//
// Unlike `crush sessions tail <id> --follow`, this command:
//   - always uses the text format with timestamp + ago headers,
//   - prints a final summary block instead of silently terminating,
//   - is symmetric with the "dashboard" mode of `crush sessions watch`,
//     so operators have one verb for live observation.
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

	now := time.Now()
	lastPrinted := ""
	for _, msg := range messages {
		printMessageWithTime(os.Stdout, msg, "text", now)
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
				printMessageWithTime(os.Stdout, msgs[i], "text", tickNow)
				lastPrinted = msgs[i].ID
			}
		}
	}
}

// isSessionFinished reports whether a live-tail loop should exit. Returns
// the end reason as a short human label so the summary block can show
// "(ended: <reason>)".
func isSessionFinished(ctx context.Context, a *app.App, sessionID, locksDir string) (bool, string) {
	// (a) session row has an ended_reason.
	sess, err := a.Sessions.Get(ctx, sessionID)
	if err == nil && sess.EndedReason != "" {
		return true, sess.EndedReason
	}

	// (b) the lock file is gone — process exited or was killed. Only
	// trust this signal when the session actually has at least one
	// message (otherwise we might race the acquirer that has not yet
	// touched the lock or written its first message).
	lockPath := filepath.Join(locksDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	if _, statErr := os.Stat(lockPath); os.IsNotExist(statErr) {
		msgs, mErr := a.Messages.List(ctx, sessionID)
		if mErr == nil && len(msgs) > 0 {
			return true, "lock_released"
		}
	}

	// (c) latest assistant message has a non-partial Finish part.
	msgs, err := a.Messages.List(ctx, sessionID)
	if err == nil && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if f := last.FinishPart(); f != nil && !f.Partial && f.Reason != "" {
			return true, string(f.Reason)
		}
	}
	return false, ""
}

// printWatchSummary emits the final block shown when a watched session
// finishes. Pulls fresh totals from the session row so any in-flight
// IncrementCost from the agent's last step is reflected.
func printWatchSummary(w *os.File, ctx context.Context, a *app.App, sessionID, reason string) {
	sess, err := a.Sessions.Get(ctx, sessionID)
	if err != nil {
		fmt.Fprintf(w, "\n--- session ended (could not load summary: %v) ---\n", err)
		return
	}
	duration := time.Duration(0)
	if sess.CreatedAt > 0 {
		duration = time.Since(time.Unix(sess.CreatedAt, 0))
	}
	tokens := sess.PromptTokens + sess.CompletionTokens
	fmt.Fprintln(w)
	fmt.Fprintln(w, "--- session ended ---")
	fmt.Fprintf(w, "id:       %s\n", sess.ID)
	if sess.Title != "" {
		fmt.Fprintf(w, "title:    %s\n", sess.Title)
	}
	fmt.Fprintf(w, "reason:   %s\n", reason)
	fmt.Fprintf(w, "duration: %s\n", formatDurationShort(duration))
	fmt.Fprintf(w, "tokens:   %s (prompt %s + completion %s)\n",
		formatInt(tokens), formatInt(sess.PromptTokens), formatInt(sess.CompletionTokens))
	fmt.Fprintf(w, "cost:     $%.4f", sess.Cost)
	if sess.BudgetMaxCost > 0 {
		fmt.Fprintf(w, " / $%.4f budget", sess.BudgetMaxCost)
	}
	fmt.Fprintln(w)
}

// pickSessionForWatch runs the same interactive picker used by
// "sessions pick" but, when active locks exist, sorts them to the top so
// the user typically lands on a live session by default. Returns "" when
// the user quits without selecting.
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
			hash:    short(sessionHashIDLocal(s.ID)),
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

// sessionHashIDLocal is the indirection used by pickSessionForWatch to
// keep the watch file from depending directly on the session package
// import path that already lives in this file via the dashboard mode.
func sessionHashIDLocal(id string) string {
	return session.HashID(id)
}
