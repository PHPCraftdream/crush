package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	Short: "List and manage sessions stored in this workspace",
	Long: `Sessions are the unit of conversation context. The web UI and
"crush run" both create / continue them; this subcommand exposes the same
store from the CLI so scripts can enumerate, delete, or reset them
without poking at the SQLite file directly.`,
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

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, s := range sessions {
				if err := enc.Encode(makeSessionListItem(s)); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "HASH\tID\tTITLE\tMSGS\tUPDATED\tTOKENS\tCOST")
		for _, s := range sessions {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t$%.4f\n",
				short(session.HashID(s.ID)),
				s.ID,
				truncate(s.Title, 40),
				s.MessageCount,
				time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04"),
				s.PromptTokens+s.CompletionTokens,
				s.Cost,
			)
		}
		return tw.Flush()
	},
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
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
		if err != nil {
			return err
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
		sess.MessageCount = 0
		sess.PromptTokens = 0
		sess.CompletionTokens = 0
		sess.Cost = 0
		sess.SummaryMessageID = ""
		if _, err := a.Sessions.Save(cmd.Context(), sess); err != nil {
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
without cleanup. This command does NOT delete locks automatically — use
external cleanup if needed.

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
	if format != "text" && format != "ndjson" {
		return fmt.Errorf("invalid format: %s (must be text or ndjson)", format)
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sessionID := args[0]
	if _, err := resolveSessionID(cmd.Context(), a.Sessions, sessionID); err != nil {
		return err
	}

	messages, err := a.Messages.List(cmd.Context(), sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if len(messages) > n {
		messages = messages[len(messages)-n:]
	}
	for _, msg := range messages {
		printMessage(os.Stdout, msg, format)
	}
	return nil
}

func init() {
	sessionsListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")

	sessionsShowCmd.Flags().Bool("json", false, "Emit structured JSON instead of text")
	sessionsShowCmd.Flags().Bool("with-messages", false, "Include all messages in the output")
	sessionsShowCmd.Flags().Bool("full", false, "Show full message content (implies --with-messages)")

	sessionsLocksCmd.Flags().Bool("json", false, "Emit NDJSON (one JSON object per line)")
	sessionsLocksCmd.Flags().Bool("stale-only", false, "Filter to locks older than 10 minutes or for dead processes")

	sessionsTailCmd.Flags().Bool("follow", false, "Keep polling for new messages until session finishes")
	sessionsTailCmd.Flags().String("from-message", "", "Resume from this message ID (skip earlier)")
	sessionsTailCmd.Flags().String("format", "text", "Output format: text or ndjson")

	sessionsLastCmd.Flags().IntP("n", "n", 10, "Number of messages to show")
	sessionsLastCmd.Flags().String("format", "text", "Output format: text or ndjson")

	sessionsCmd.AddCommand(sessionsListCmd, sessionsDeleteCmd, sessionsResetCmd, sessionsShowCmd, sessionsLocksCmd, sessionsTailCmd, sessionsLastCmd)
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
	if full {
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
		ID         string `json:"id"`
		Role       string `json:"role"`
		Preview    string `json:"preview"`
		FinishReason string `json:"finish_reason,omitempty"`
	}

	type sessionShowOutput struct {
		ID               string    `json:"id"`
		Hash             string    `json:"hash"`
		Title            string    `json:"title"`
		ParentID         string    `json:"parent_id,omitempty"`
		Provider         string    `json:"provider,omitempty"`
		Model            string    `json:"model,omitempty"`
		Effort           string    `json:"effort,omitempty"`
		CreatedAt        int64     `json:"created_at"`
		UpdatedAt        int64     `json:"updated_at"`
		MessageCount     int64     `json:"message_count"`
		PromptTokens     int64     `json:"prompt_tokens"`
		CompletionTokens int64     `json:"completion_tokens"`
		CostUSD          float64   `json:"cost_usd"`
		SystemPrompt     string    `json:"system_prompt,omitempty"`
		Messages         []msgItem `json:"messages,omitempty"`
	}

	output := sessionShowOutput{
		ID:               sess.ID,
		Hash:             session.HashID(sess.ID),
		Title:            sess.Title,
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
		SystemPrompt:     sess.SystemPrompt,
	}

	if withMessages {
		messages, err := a.Messages.List(cmd.Context(), sess.ID)
		if err != nil {
			return fmt.Errorf("failed to list messages: %w", err)
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
			if f := msg.FinishPart(); f != nil {
				finishReason = string(f.Reason)
			}

			output.Messages[i] = msgItem{
				ID:           msg.ID,
				Role:         string(msg.Role),
				Preview:      preview,
				FinishReason: finishReason,
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
	fmt.Printf("Cost:         $%.6f USD\n", output.CostUSD)
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
				fmt.Printf("     (finished: %s)\n", msg.FinishReason)
			}
		}
	}

	return nil
}

// lockPulseStatus classifies a lock file by its heartbeat mtime.
// Heartbeat interval = 10s, stale threshold = 20s (session.lockStaleDuration).
//   0–10s  → "alive"    (fresh heartbeat)
//   10–15s → "ping"     (one beat overdue, likely OK)
//   15–20s → "stopping" (two beats missed, probably finishing)
//   >20s   → "offline"  (stale — holder crashed or exited without Release)
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

func sessionsLocksCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	staleOnly, _ := cmd.Flags().GetBool("stale-only")

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
	}

	var locks []lockItem
	now := time.Now()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}

		sessionID := strings.TrimPrefix(entry.Name(), "session-")
		sessionID = strings.TrimSuffix(sessionID, ".lock")

		info, _ := entry.Info()
		pulseSec := int64(now.Sub(info.ModTime()).Seconds())
		pulse := lockPulseStatus(pulseSec)
		stale := pulse == "offline"

		if staleOnly && !stale {
			continue
		}

		lockPath := filepath.Join(locksDir, entry.Name())
		pidBytes, _ := os.ReadFile(lockPath)
		pid := 0
		fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &pid)

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
	fmt.Fprintln(tw, "SESSION_ID\tPID\tPULSE\tPULSE_AGE\tDURATION")
	for _, lock := range locks {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%ds ago\t%ds\n",
			truncate(lock.SessionID, 28),
			lock.PID,
			lock.Pulse,
			lock.PulseSec,
			lock.DurationSec,
		)
	}
	return tw.Flush()
}

func sessionsTailCmdRun(cmd *cobra.Command, args []string) error {
	follow, _ := cmd.Flags().GetBool("follow")
	fromMsgID, _ := cmd.Flags().GetString("from-message")
	format, _ := cmd.Flags().GetString("format")

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

	// Print messages
	for _, msg := range messages {
		printMessage(os.Stdout, msg, format)
		lastPrinted = msg.ID
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

		// Print any new messages
		for i := range messages {
			if messages[i].ID != lastPrinted && (lastPrinted == "" || isAfter(&messages[i], findByID(messages, lastPrinted))) {
				printMessage(os.Stdout, messages[i], format)
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

func findByID(messages []message.Message, id string) *message.Message {
	for i := range messages {
		if messages[i].ID == id {
			return &messages[i]
		}
	}
	return nil
}

func isAfter(a, b *message.Message) bool {
	if b == nil {
		return true
	}
	return a.CreatedAt > b.CreatedAt || (a.CreatedAt == b.CreatedAt && a.ID > b.ID)
}

func printMessage(w io.Writer, msg message.Message, format string) {
	if format == "ndjson" {
		type msgJSON struct {
			ID           string `json:"id"`
			Role         string `json:"role"`
			Preview      string `json:"preview"`
			FinishReason string `json:"finish_reason,omitempty"`
		}
		preview := ""
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok {
				preview = truncate(tc.Text, 200)
				break
			}
		}
		finishReason := ""
		if f := msg.FinishPart(); f != nil {
			finishReason = string(f.Reason)
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(msgJSON{
			ID:           msg.ID,
			Role:         string(msg.Role),
			Preview:      preview,
			FinishReason: finishReason,
		})
	} else {
		// text format
		fmt.Fprintf(w, "[%s]\n", msg.Role)
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok {
				fmt.Fprintf(w, "%s\n", tc.Text)
			}
		}
		if f := msg.FinishPart(); f != nil && f.Reason != "" {
			fmt.Fprintf(w, "(finished: %s)\n", f.Reason)
		}
		fmt.Fprintf(w, "\n")
	}
}
