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

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/spf13/cobra"
)

var sessionsWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Live dashboard of active sessions",
	Long: `Poll every --interval (default 3s) and display a table of sessions
that have a live lock file in .crush/locks/.

Columns:
  SESSION_ID  — truncated to 24 chars
  PULSE       — alive / ping / stopping / offline (from lock mtime)
  AGE         — time since session was created
  LAST_TOOL   — name of the last ToolCall in the most recent assistant message
  TOKENS      — total prompt + completion tokens
  COST        — cost formatted as $X.XXX

Exits on Ctrl+C. Use --json for NDJSON output (one JSON array per poll).`,
	Example: `
# Live dashboard with default 3s refresh
crush sessions watch

# Faster refresh
crush sessions watch --interval 1s

# Machine-readable NDJSON output
crush sessions watch --json
  `,
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
	sessionsWatchCmd.Flags().Bool("json", false, "Emit NDJSON: one JSON array per poll cycle")
}
