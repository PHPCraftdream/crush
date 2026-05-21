package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsPurgeCmd = &cobra.Command{
	Use:   "purge <age>",
	Short: "Delete ALL sessions older than the given age",
	Long: `Delete every session whose CreatedAt is older than <age>.

Unlike "sessions gc" (which only removes empty/orphaned sessions), purge is
unconditional — use with care. Pair with --dry-run first.

Supported age suffixes:
  s, sec       seconds        e.g. 30s
  min          minutes        e.g. 15min
  h            hours          e.g. 3h
  d            days           e.g. 3d
  w            weeks          e.g. 2w
  m, mo, mon   months (30d)   e.g. 1m
  y           years (365d)    e.g. 1y`,
	Example: `
crush sessions purge 1m --dry-run
crush sessions purge 3d
crush sessions purge 15min --yes
  `,
	Args: cobra.ExactArgs(1),
	RunE: sessionsPurgeCmdRun,
}

func sessionsPurgeCmdRun(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	yes, _ := cmd.Flags().GetBool("yes")

	age, err := parseAge(args[0])
	if err != nil {
		return err
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

	cutoff := time.Now().Add(-age)
	var victims []session.Session
	for _, s := range sessions {
		if time.Unix(s.CreatedAt, 0).Before(cutoff) {
			victims = append(victims, s)
		}
	}

	if len(victims) == 0 {
		fmt.Fprintln(os.Stderr, "(no sessions older than", args[0]+")")
		return nil
	}

	if !dryRun && !yes {
		fmt.Fprintf(os.Stderr, "About to delete %d session(s) older than %s. Re-run with --yes to confirm or --dry-run to preview.\n", len(victims), args[0])
		return nil
	}

	for _, s := range victims {
		action := "would delete"
		if !dryRun {
			action = "deleted"
		}
		fmt.Fprintf(os.Stderr, "%s session %s (%s) created %s\n",
			action, short(session.HashID(s.ID)), truncate(s.Title, 40),
			time.Unix(s.CreatedAt, 0).Format(time.RFC3339))

		if !dryRun {
			if err := a.Sessions.Delete(cmd.Context(), s.ID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to delete %s: %v\n", s.ID, err)
			}
		}
	}

	prefix := "would delete"
	if !dryRun {
		prefix = "deleted"
	}
	fmt.Fprintf(os.Stderr, "%s %d session(s)\n", prefix, len(victims))
	return nil
}

// parseAge parses durations with extended suffixes: s/sec, min, h, d, w,
// m/mo/mon (=30d), y (=365d). Pure-Go time.ParseDuration treats "m" as
// minutes; we intentionally override that here because "1m" almost always
// means "1 month" in the session-lifetime context.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	i := 0
	for i < len(s) && (unicode.IsDigit(rune(s[i])) || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid duration %q: missing number", s)
	}
	numStr, unit := s[:i], strings.ToLower(s[i:])

	var n float64
	if _, err := fmt.Sscanf(numStr, "%f", &n); err != nil {
		return 0, fmt.Errorf("invalid number in %q: %w", s, err)
	}

	var mult time.Duration
	switch unit {
	case "s", "sec":
		mult = time.Second
	case "min":
		mult = time.Minute
	case "h":
		mult = time.Hour
	case "d":
		mult = 24 * time.Hour
	case "w":
		mult = 7 * 24 * time.Hour
	case "m", "mo", "mon":
		mult = 30 * 24 * time.Hour
	case "y":
		mult = 365 * 24 * time.Hour
	case "":
		return 0, fmt.Errorf("invalid duration %q: missing unit (expected s/min/h/d/w/m/y)", s)
	default:
		return 0, fmt.Errorf("invalid duration %q: unknown unit %q", s, unit)
	}

	d := time.Duration(n * float64(mult))
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive, got %q", s)
	}
	return d, nil
}

func init() {
	sessionsPurgeCmd.Flags().Bool("dry-run", false, "Print what would be deleted without deleting")
	sessionsPurgeCmd.Flags().Bool("yes", false, "Skip the confirmation prompt")
}
