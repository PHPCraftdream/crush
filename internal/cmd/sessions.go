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
		statusByID := computeSessionStatuses(cmd)

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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t$%.4f\n",
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
func computeSessionStatuses(cmd *cobra.Command) map[string]string {
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil
	}
	locksDir := filepath.Join(cwd, ".crush", "locks")
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
		if pid > 0 && session.IsProcessAlive(pid) {
			out[sessionID] = "running"
		} else {
			out[sessionID] = "crashed"
		}
	}
	return out
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
		// Uses the shared forceKillHolder + removeLockWithRetry helpers
		// (defined in sessions_kill.go) so kill / wait-for-death /
		// retry-remove behaves identically here and in `sessions kill`.
		// On Windows the kill goes through taskkill /F /T which also
		// terminates the spawned CLI subprocess tree.
		if force {
			cwd, err := ResolveCwd(cmd)
			if err == nil {
				lockPath := filepath.Join(cwd, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
				if _, statErr := os.Stat(lockPath); statErr == nil {
					pid := session.ReadLockPID(lockPath)
					fmt.Fprint(os.Stderr, forceKillHolder(pid, 5*time.Second))
					if err := removeLockWithRetry(lockPath, 5*time.Second); err != nil {
						fmt.Fprintf(os.Stderr, "warning: could not remove lock %s: %v\n", lockPath, err)
					} else {
						fmt.Fprintf(os.Stderr, "removed lock %s\n", lockPath)
					}
				}
			}
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
	for _, msg := range messages {
		printMessageWithTime(os.Stdout, msg, format, now, callCtx)
	}
	return nil
}

func init() {
	sessionsListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")

	sessionsResetCmd.Flags().Bool("force", false, "Also kill any process holding the session lock and remove the lock file")

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

	sessionsCmd.AddCommand(sessionsListCmd, sessionsDeleteCmd, sessionsResetCmd, sessionsShowCmd, sessionsLocksCmd, sessionsTailCmd, sessionsLastCmd, sessionsGcCmd, sessionsPurgeCmd, sessionsKillCmd, sessionsReapCmd, sessionsWatchCmd, sessionsPickCmd, sessionsGrepCmd, sessionsCostCmd, sessionsDiffCmd, sessionsCancelCmd, sessionsForkCmd, sessionsTreeCmd)
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
		ID           string `json:"id"`
		Role         string `json:"role"`
		Preview      string `json:"preview"`
		FinishReason string `json:"finish_reason,omitempty"`
	}

	type sessionShowOutput struct {
		ID               string    `json:"id"`
		Hash             string    `json:"hash"`
		Title            string    `json:"title"`
		Purpose          string    `json:"purpose,omitempty"` // first user prompt excerpt
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
		EndedReason      string    `json:"ended_reason,omitempty"`
		BudgetMaxCost    float64   `json:"budget_max_cost,omitempty"`
		BudgetMaxTokens  int64     `json:"budget_max_tokens,omitempty"`
		BudgetTimeoutSec int64     `json:"budget_timeout_sec,omitempty"`
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
				fmt.Printf("     (finished: %s)\n", msg.FinishReason)
			}
		}
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
		SessionID    string `json:"session_id"`
		PID          int    `json:"pid"`
		PulseSec     int64  `json:"pulse_sec"`
		Pulse        string `json:"pulse"` // alive / ping / stopping / offline
		AcquiredAt   int64  `json:"acquired_at_unix"`
		DurationSec  int64  `json:"duration_seconds"`
		Stale        bool   `json:"stale"`
		BudgetSec    int64  `json:"budget_sec,omitempty"` // --timeout seconds, 0 if not set
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

		// Auto-delete locks older than 1 minute — heartbeat would have
		// touched the file every 10s if the holder were alive.
		if age > autoDeleteAfter {
			if err := os.Remove(lockPath); err == nil {
				fmt.Fprintf(os.Stderr, "removed stale lock %s (age %ds)\n", entry.Name(), int(age.Seconds()))
			}
			continue
		}

		pulseSec := int64(age.Seconds())
		pulse := lockPulseStatus(pulseSec)
		stale := pulse == "offline"

		if staleOnly && !stale {
			continue
		}

		pidBytes, _ := os.ReadFile(lockPath)
		pid := 0
		fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &pid)
		budgetSec := session.ReadLockTimeoutSec(lockPath)

		// Approximate acquire time: mtime when pulse was fresh.
		// For alive locks mtime ≈ now, so we use file birthtime via stat
		// if available; otherwise mtime is the best proxy.
		acqTime := info.ModTime().Unix()
		duration := int64(now.Sub(info.ModTime()).Seconds())

		locks = append(locks, lockItem{
			SessionID:    sessionID,
			PID:          pid,
			PulseSec:     pulseSec,
			Pulse:        pulse,
			AcquiredAt:   acqTime,
			DurationSec:  duration,
			Stale:        stale,
			BudgetSec:    budgetSec,
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
	fmt.Fprintln(tw, "SESSION_ID\tPID\tPULSE\tPULSE_AGE\tELAPSED\tBUDGET")
	for _, lock := range locks {
		budget := "∞"
		if lock.BudgetSec > 0 {
			budget = formatDurationShort(time.Duration(lock.BudgetSec) * time.Second)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%ds ago\t%s\t%s\n",
			truncate(lock.SessionID, 28),
			lock.PID,
			lock.Pulse,
			lock.PulseSec,
			formatDurationShort(time.Duration(lock.DurationSec)*time.Second),
			budget,
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

	// Build origin context from the snapshot we have right now; the
	// follow loop below extends it as new ToolCall parts arrive.
	callCtx := buildToolCallContext(messages)

	// Print messages
	now := time.Now()
	for _, msg := range messages {
		printMessageWithTime(os.Stdout, msg, format, now, callCtx)
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

		// Rebuild origin context — new ToolCall parts may have arrived
		// this tick, and the next ToolResult render needs them.
		callCtx = buildToolCallContext(messages)

		// Print any new messages
		now := time.Now()
		for i := range messages {
			if messages[i].ID != lastPrinted && (lastPrinted == "" || isAfter(&messages[i], findByID(messages, lastPrinted))) {
				printMessageWithTime(os.Stdout, messages[i], format, now, callCtx)
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
func printMessageWithTime(w io.Writer, msg message.Message, format string, now time.Time, callCtx map[string]toolCallOrigin) {
	if format == "text" && msg.CreatedAt != 0 {
		ts := time.Unix(msg.CreatedAt, 0)
		ago := now.Sub(ts)
		fmt.Fprintf(w, "[%s] (%s)\n", ts.Format("2006-01-02 15:04:05"), formatAgo(ago))
	}
	printMessage(w, msg, format, callCtx)
}

func printMessage(w io.Writer, msg message.Message, format string, callCtx map[string]toolCallOrigin) {
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
			switch p := part.(type) {
			case message.TextContent:
				fmt.Fprintf(w, "%s\n", p.Text)
			case message.ToolCall:
				if preview := formatToolCallPreview(p.Name, p.Input); preview != "" {
					fmt.Fprintf(w, "[tool: %s] %s\n", p.Name, preview)
				} else {
					fmt.Fprintf(w, "[tool: %s]\n", p.Name)
				}
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
			}
		}
		if f := msg.FinishPart(); f != nil && f.Reason != "" {
			fmt.Fprintf(w, "(finished: %s)\n", f.Reason)
		}
		fmt.Fprintf(w, "\n")
	}
}
