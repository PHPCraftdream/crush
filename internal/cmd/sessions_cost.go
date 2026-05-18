package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsCostCmd = &cobra.Command{
	Use:   "cost",
	Short: "Show cost breakdown across sessions",
	Long: `Show cost and token usage broken down by model, day, or session.

By default groups by model. Use --by to change the grouping:
  model   — group by LargeModelID (default)
  day     — group by date (YYYY-MM-DD)
  session — show per-session breakdown (top N by cost)
  total   — just print the grand total

Use --since to filter to sessions updated within the given duration.
Supports Go durations (30m, 24h), day suffix (7d), or plain integers
(interpreted as days). Default: show all sessions.`,
	Example: `
# Cost grouped by model
crush sessions cost

# Cost grouped by day
crush sessions cost --by day

# Last 7 days, grouped by model
crush sessions cost --since 7d

# Top 20 most expensive sessions
crush sessions cost --by session --top 20

# Machine-readable output
crush sessions cost --json | jq '.[] | select(.cost_usd > 1.0)'
  `,
	RunE: sessionsCostCmdRun,
}

func sessionsCostCmdRun(cmd *cobra.Command, args []string) error {
	sinceStr, _ := cmd.Flags().GetString("since")
	by, _ := cmd.Flags().GetString("by")
	asJSON, _ := cmd.Flags().GetBool("json")
	topN, _ := cmd.Flags().GetInt("top")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sessions, err := a.Sessions.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Filter out child sessions.
	visible := sessions[:0]
	for _, s := range sessions {
		if s.ParentSessionID != "" {
			continue
		}
		visible = append(visible, s)
	}
	sessions = visible

	// Apply --since filter.
	if sinceStr != "" {
		sinceDur, err := parseSinceDuration(sinceStr)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		cutoff := time.Now().Add(-sinceDur).Unix()
		filtered := sessions[:0]
		for _, s := range sessions {
			if s.UpdatedAt >= cutoff {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	if len(sessions) == 0 {
		if asJSON {
			fmt.Println("[]")
		} else {
			fmt.Println("(no sessions)")
		}
		return nil
	}

	switch by {
	case "model", "":
		return costByModel(sessions, asJSON)
	case "day":
		return costByDay(sessions, asJSON)
	case "session":
		return costBySession(sessions, asJSON, topN)
	case "total":
		return costTotal(sessions, asJSON)
	default:
		return fmt.Errorf("--by: invalid value %q (allowed: model|day|session|total)", by)
	}
}

// parseSinceDuration extends parseDurationDays to also support plain integers
// as days.
func parseSinceDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		return parseDurationDays(s)
	}
	// Try plain integer as days.
	if n, err := parsePlainInt(s); err == nil {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func parsePlainInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

type costRow struct {
	Key      string  `json:"key"`
	Sessions int     `json:"sessions"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

func costByModel(sessions []session.Session, asJSON bool) error {
	groups := make(map[string]*costRow)
	var keys []string
	var totalTokens int64
	var totalCost float64
	var totalSessions int

	for _, s := range sessions {
		model := s.LargeModelID
		if model == "" {
			model = "(unknown)"
		}
		row, ok := groups[model]
		if !ok {
			row = &costRow{Key: model}
			groups[model] = row
			keys = append(keys, model)
		}
		row.Sessions++
		row.Tokens += s.PromptTokens + s.CompletionTokens
		row.CostUSD += s.Cost
		totalTokens += s.PromptTokens + s.CompletionTokens
		totalCost += s.Cost
		totalSessions++
	}

	sort.Slice(keys, func(i, j int) bool {
		return groups[keys[i]].CostUSD > groups[keys[j]].CostUSD
	})

	if asJSON {
		var rows []costRow
		for _, k := range keys {
			rows = append(rows, *groups[k])
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(rows)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tSESSIONS\tTOKENS\tCOST")
	for _, k := range keys {
		row := groups[k]
		fmt.Fprintf(tw, "%s\t%d\t%s\t$%.3f\n",
			k, row.Sessions, formatInt64(row.Tokens), row.CostUSD)
	}
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
		strings.Repeat("─", 53), "", "", "")
	fmt.Fprintf(tw, "TOTAL\t%d\t%s\t$%.3f\n",
		totalSessions, formatInt64(totalTokens), totalCost)
	return tw.Flush()
}

func costByDay(sessions []session.Session, asJSON bool) error {
	groups := make(map[string]*costRow)
	var keys []string
	var totalTokens int64
	var totalCost float64
	var totalSessions int

	for _, s := range sessions {
		day := time.Unix(s.UpdatedAt, 0).Format("2006-01-02")
		row, ok := groups[day]
		if !ok {
			row = &costRow{Key: day}
			groups[day] = row
			keys = append(keys, day)
		}
		row.Sessions++
		row.Tokens += s.PromptTokens + s.CompletionTokens
		row.CostUSD += s.Cost
		totalTokens += s.PromptTokens + s.CompletionTokens
		totalCost += s.Cost
		totalSessions++
	}

	sort.Strings(keys)

	if asJSON {
		var rows []costRow
		for _, k := range keys {
			rows = append(rows, *groups[k])
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(rows)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DATE\tSESSIONS\tTOKENS\tCOST")
	for _, k := range keys {
		row := groups[k]
		fmt.Fprintf(tw, "%s\t%d\t%s\t$%.3f\n",
			k, row.Sessions, formatInt64(row.Tokens), row.CostUSD)
	}
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
		strings.Repeat("─", 53), "", "", "")
	fmt.Fprintf(tw, "TOTAL\t%d\t%s\t$%.3f\n",
		totalSessions, formatInt64(totalTokens), totalCost)
	return tw.Flush()
}

func costBySession(sessions []session.Session, asJSON bool, topN int) error {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Cost > sessions[j].Cost
	})

	if topN > 0 && len(sessions) > topN {
		sessions = sessions[:topN]
	}

	if asJSON {
		type sessionCost struct {
			ID        string  `json:"id"`
			Title     string  `json:"title"`
			Tokens    int64   `json:"tokens"`
			CostUSD   float64 `json:"cost_usd"`
			UpdatedAt int64   `json:"updated_at"`
		}
		var rows []sessionCost
		for _, s := range sessions {
			rows = append(rows, sessionCost{
				ID:        s.ID,
				Title:     s.Title,
				Tokens:    s.PromptTokens + s.CompletionTokens,
				CostUSD:   s.Cost,
				UpdatedAt: s.UpdatedAt,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(rows)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tTOKENS\tCOST")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t$%.3f\n",
			s.ID, truncate(s.Title, 40), formatInt64(s.PromptTokens+s.CompletionTokens), s.Cost)
	}
	return tw.Flush()
}

func costTotal(sessions []session.Session, asJSON bool) error {
	var totalTokens int64
	var totalCost float64
	for _, s := range sessions {
		totalTokens += s.PromptTokens + s.CompletionTokens
		totalCost += s.Cost
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(map[string]any{
			"sessions": len(sessions),
			"tokens":   totalTokens,
			"cost_usd": totalCost,
		})
	}

	fmt.Printf("Sessions:  %d\n", len(sessions))
	fmt.Printf("Tokens:    %s\n", formatInt64(totalTokens))
	fmt.Printf("Cost:      $%.3f\n", totalCost)
	return nil
}

// formatInt64 formats an integer with comma separators.
func formatInt64(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteByte(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

func init() {
	sessionsCostCmd.Flags().String("since", "", "Only include sessions updated within this duration (e.g. 7d, 24h, 30m, 3)")
	sessionsCostCmd.Flags().String("by", "model", "Grouping: model|day|session|total")
	sessionsCostCmd.Flags().Bool("json", false, "Emit JSON output")
	sessionsCostCmd.Flags().Int("top", 20, "Number of sessions to show with --by session")
}
