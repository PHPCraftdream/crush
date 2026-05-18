package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsGrepCmd = &cobra.Command{
	Use:   "grep <pattern>",
	Short: "Search message text across sessions",
	Long: `Search message text content across all sessions (or a specific one).

The pattern is matched as a case-insensitive substring by default. If the
pattern starts and ends with '/' (e.g. /TODO|FIXME/), it is treated as a
regular expression.

For each matching message the output shows the session title, message id,
role, timestamp, and the matching text lines.

Use --n to control the number of context lines around the match (default 3).
Use --role to filter by message role (assistant or user).
Use --session to search only within a specific session.
Use --json to emit one JSON object per match suitable for piping into jq.`,
	Example: `
# Find all TODO mentions across sessions
crush sessions grep "TODO"

# Regex search
crush sessions grep "/TODO|FIXME/"

# Search within a specific session
crush sessions grep "error" --session myid-123

# Filter to assistant messages only
crush sessions grep "refactor" --role assistant

# Machine-readable output
crush sessions grep "TODO" --json | jq '.excerpt'
  `,
	Args: cobra.ExactArgs(1),
	RunE: sessionsGrepCmdRun,
}

type grepMatch struct {
	SessionID    string `json:"session_id"`
	SessionTitle string `json:"session_title"`
	MessageID    string `json:"message_id"`
	Role         string `json:"role"`
	CreatedAt    int64  `json:"created_at"`
	Excerpt      string `json:"excerpt"`
}

func sessionsGrepCmdRun(cmd *cobra.Command, args []string) error {
	pattern := args[0]
	n, _ := cmd.Flags().GetInt("n")
	sessionFilter, _ := cmd.Flags().GetString("session")
	roleFilter, _ := cmd.Flags().GetString("role")
	asJSON, _ := cmd.Flags().GetBool("json")

	if roleFilter != "" && roleFilter != "assistant" && roleFilter != "user" {
		return fmt.Errorf("--role: invalid value %q (allowed: assistant|user)", roleFilter)
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	ctx := cmd.Context()

	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Build the matcher.
	matcher, err := buildGrepMatcher(pattern)
	if err != nil {
		return fmt.Errorf("invalid pattern: %w", err)
	}

	// If a specific session is requested, resolve it.
	if sessionFilter != "" {
		sess, err := resolveSessionID(ctx, a.Sessions, sessionFilter)
		if err != nil {
			return err
		}
		sessions = []session.Session{{ID: sess.ID, Title: sess.Title}}
	}

	now := time.Now()
	var totalMatches int

	for _, sess := range sessions {
		msgs, err := a.Messages.List(ctx, sess.ID)
		if err != nil {
			continue
		}

		for _, msg := range msgs {
			if roleFilter != "" && string(msg.Role) != roleFilter {
				continue
			}

			for _, part := range msg.Parts {
				tc, ok := part.(message.TextContent)
				if !ok {
					continue
				}

				lines := strings.Split(tc.Text, "\n")
				var matchingLineIndices []int
				for i, line := range lines {
					if matcher(line) {
						matchingLineIndices = append(matchingLineIndices, i)
					}
				}
				if len(matchingLineIndices) == 0 {
					continue
				}

				// Build excerpt from context lines around matches.
				excerpt := buildExcerpt(lines, matchingLineIndices, n)
				totalMatches++

				if asJSON {
					enc := json.NewEncoder(os.Stdout)
					_ = enc.Encode(grepMatch{
						SessionID:    sess.ID,
						SessionTitle: sess.Title,
						MessageID:    msg.ID,
						Role:         string(msg.Role),
						CreatedAt:    msg.CreatedAt,
						Excerpt:      excerpt,
					})
				} else {
					ts := time.Unix(sess.UpdatedAt, 0).Format("2006-01-02 15:04")
					fmt.Fprintf(os.Stdout, "\n=== session: %s (%s) ===\n", truncate(sess.Title, 40), ts)
					msgTS := time.Unix(msg.CreatedAt, 0).Format("2006-01-02 15:04:05")
					ago := formatAgo(now.Sub(time.Unix(msg.CreatedAt, 0)))
					fmt.Fprintf(os.Stdout, "[%s] [%s] (%s, %s)\n", msg.ID, msg.Role, msgTS, ago)
					fmt.Fprintf(os.Stdout, "%s\n", excerpt)
				}
			}
		}
	}

	if !asJSON && totalMatches == 0 {
		fmt.Fprintln(os.Stderr, "(no matches)")
	}

	return nil
}

// buildGrepMatcher returns a function that checks if a line matches the
// pattern. Plain patterns are case-insensitive substrings; patterns
// delimited by '/' are compiled as regex.
func buildGrepMatcher(pattern string) (func(string) bool, error) {
	if len(pattern) > 2 && pattern[0] == '/' && pattern[len(pattern)-1] == '/' {
		re, err := regexp.Compile("(?i)" + pattern[1:len(pattern)-1])
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}
	lower := strings.ToLower(pattern)
	return func(s string) bool {
		return strings.Contains(strings.ToLower(s), lower)
	}, nil
}

// buildExcerpt returns the relevant lines with context around matches.
func buildExcerpt(lines []string, matchIndices []int, contextLines int) string {
	include := make(map[int]bool)
	for _, idx := range matchIndices {
		for i := idx - contextLines; i <= idx+contextLines; i++ {
			if i >= 0 && i < len(lines) {
				include[i] = true
			}
		}
	}

	var sb strings.Builder
	prev := -2
	for i := 0; i < len(lines); i++ {
		if !include[i] {
			continue
		}
		if prev >= 0 && i > prev+1 {
			sb.WriteString("...\n")
		}
		sb.WriteString(lines[i])
		sb.WriteString("\n")
		prev = i
	}
	return strings.TrimRight(sb.String(), "\n")
}

func init() {
	sessionsGrepCmd.Flags().IntP("n", "n", 3, "Number of context lines around each match")
	sessionsGrepCmd.Flags().String("session", "", "Search only within this session (id or hash prefix)")
	sessionsGrepCmd.Flags().String("role", "", "Filter by message role: assistant|user")
	sessionsGrepCmd.Flags().Bool("json", false, "Emit one JSON object per match")
}
