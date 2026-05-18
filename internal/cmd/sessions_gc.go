package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsGcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect stale sessions",
	Long: `Delete sessions that are no longer useful:

  1. Sessions older than --older-than (default 7 days) with zero messages
     or only system messages.
  2. Sessions with ID prefix "ping-" older than 1 hour.
  3. Child sessions (parent_id != "") whose parent no longer exists,
     older than 24h.

Use --dry-run to print what would be deleted without deleting.
Use --max-sessions to cap the number of deletions per run.
Use --json to emit one JSON object per deleted (or would-be-deleted) session.`,
	Example: `
# Dry run: show what would be collected
crush sessions gc --dry-run

# Collect with defaults (7 days, no limit)
crush sessions gc

# Collect sessions older than 3 days, max 50 deletions
crush sessions gc --older-than 3d --max-sessions 50

# Machine-readable output
crush sessions gc --dry-run --json
  `,
	RunE: sessionsGcCmdRun,
}

type gcItem struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	AgeHours float64 `json:"age_hours"`
	Reason   string  `json:"reason"`
}

func sessionsGcCmdRun(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	olderThanStr, _ := cmd.Flags().GetString("older-than")
	maxSessions, _ := cmd.Flags().GetInt("max-sessions")
	asJSON, _ := cmd.Flags().GetBool("json")

	olderThan, err := parseDurationDays(olderThanStr)
	if err != nil {
		return fmt.Errorf("--older-than: %w", err)
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sessions, err := a.Sessions.ListAll(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Build a set of existing session IDs for orphan detection.
	existingIDs := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		existingIDs[s.ID] = struct{}{}
	}

	now := time.Now()
	var toDelete []gcItem

	for _, s := range sessions {
		age := now.Sub(time.Unix(s.CreatedAt, 0))
		reason := classifyForGC(s, age, existingIDs, olderThan)
		if reason == "" {
			continue
		}
		toDelete = append(toDelete, gcItem{
			ID:       s.ID,
			Title:    s.Title,
			AgeHours: age.Hours(),
			Reason:   reason,
		})
	}

	// Cap deletions if --max-sessions is set.
	if maxSessions > 0 && len(toDelete) > maxSessions {
		toDelete = toDelete[:maxSessions]
	}

	if len(toDelete) == 0 {
		if !asJSON {
			fmt.Println("(nothing to collect)")
		}
		return nil
	}

	// Collect messages to determine if a session has only system messages.
	// We only need this for the "empty-or-system-only" classification which
	// is already done in classifyForGC via MessageCount == 0.

	for _, item := range toDelete {
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			if err := enc.Encode(item); err != nil {
				return err
			}
		} else {
			action := "would delete"
			if !dryRun {
				action = "deleted"
			}
			fmt.Fprintf(os.Stderr, "%s session %s (%s): %s\n", action, short(session.HashID(item.ID)), truncate(item.Title, 40), item.Reason)
		}

		if !dryRun {
			if err := a.Sessions.Delete(cmd.Context(), item.ID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to delete session %s: %v\n", item.ID, err)
			}
		}
	}

	if !asJSON {
		prefix := "would delete"
		if !dryRun {
			prefix = "deleted"
		}
		fmt.Fprintf(os.Stderr, "%s %d session(s)\n", prefix, len(toDelete))
	}

	return nil
}

// classifyForGC returns a human-readable reason if the session should be
// garbage-collected, or "" if it should be kept.
func classifyForGC(s session.Session, age time.Duration, existingIDs map[string]struct{}, olderThan time.Duration) string {
	// Rule 2: ping- sessions older than 1 hour.
	if strings.HasPrefix(s.ID, "ping-") && age > 1*time.Hour {
		return "ping session older than 1 hour"
	}

	// Rule 3: orphaned child sessions older than 24h.
	if s.ParentSessionID != "" {
		if _, ok := existingIDs[s.ParentSessionID]; !ok && age > 24*time.Hour {
			return "orphaned child session (parent deleted)"
		}
	}

	// Rule 1: old sessions with zero messages.
	if age > olderThan && s.MessageCount == 0 {
		return "empty session older than threshold"
	}

	return ""
}

// parseDurationDays parses a duration string like "7d", "3d", "24h", or
// standard Go duration formats.
func parseDurationDays(s string) (time.Duration, error) {
	if s == "" {
		return 7 * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		var d int
		if _, err := fmt.Sscanf(days, "%d", &d); err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}

func init() {
	sessionsGcCmd.Flags().Bool("dry-run", false, "Print what would be deleted without deleting")
	sessionsGcCmd.Flags().String("older-than", "7d", "Delete empty sessions older than this (e.g. 7d, 24h, 30m)")
	sessionsGcCmd.Flags().Int("max-sessions", 0, "Maximum number of sessions to delete (0 = unlimited)")
	sessionsGcCmd.Flags().Bool("json", false, "Emit one JSON object per deleted session")
}
