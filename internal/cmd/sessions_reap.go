package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsReapCmd = &cobra.Command{
	Use:   "reap",
	Short: "Aggressively remove orphan lock files (dead holder PID)",
	Long: `Scan .crush/locks/ and immediately remove any lock whose holder
PID is no longer a running process. Unlike "sessions locks" — which
relies on a 60-second mtime threshold — reap acts on the PID-liveness
signal as soon as the lock is at least one heartbeat old (>10s),
matching what the run-time auto-reclaim does on the next "crush run".

Use this when you have a backlog of stuck sessions after a crash, a
TaskStop, or a reboot, and you want a single explicit sweep instead
of waiting for the next "crush run" to do it lazily.

Use --dry-run to see what would be reclaimed without touching anything.
Use --all to also remove locks whose PID file is unreadable (zero/garbage).`,
	Example: `
crush sessions reap
crush sessions reap --dry-run
crush sessions reap --all      # also nuke locks with unreadable PIDs
  `,
	RunE: sessionsReapCmdRun,
}

type reapItem struct {
	Path    string
	PID     int
	AgeSec  int
	Action  string // "remove-dead", "remove-unreadable", "skip-alive", "skip-young"
}

func sessionsReapCmdRun(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	all, _ := cmd.Flags().GetBool("all")

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	locksDir := filepath.Join(cwd, ".crush", "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "(no locks directory)")
			return nil
		}
		return err
	}

	const heartbeatThreshold = 10 * time.Second
	now := time.Now()
	items := []reapItem{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		path := filepath.Join(locksDir, entry.Name())
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		item := reapItem{Path: path, AgeSec: int(age.Seconds())}

		// Too young: the acquirer may still be writing the PID.
		if age < heartbeatThreshold {
			item.Action = "skip-young"
			items = append(items, item)
			continue
		}

		pid := session.ReadLockPID(path)
		item.PID = pid

		switch {
		case pid <= 0:
			if all {
				item.Action = "remove-unreadable"
			} else {
				item.Action = "skip-unreadable"
			}
		case session.IsProcessAlive(pid):
			item.Action = "skip-alive"
		default:
			item.Action = "remove-dead"
		}
		items = append(items, item)
	}

	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "(no locks)")
		return nil
	}

	removed := 0
	for _, it := range items {
		switch it.Action {
		case "remove-dead":
			action := "would remove"
			if !dryRun {
				if err := os.Remove(it.Path); err == nil {
					action = "removed"
					removed++
				} else {
					action = "failed to remove (" + err.Error() + ")"
				}
			}
			fmt.Fprintf(os.Stderr, "%s orphan lock %s (PID %d dead, age %ds)\n",
				action, filepath.Base(it.Path), it.PID, it.AgeSec)
		case "remove-unreadable":
			action := "would remove"
			if !dryRun {
				if err := os.Remove(it.Path); err == nil {
					action = "removed"
					removed++
				} else {
					action = "failed to remove (" + err.Error() + ")"
				}
			}
			fmt.Fprintf(os.Stderr, "%s unreadable lock %s (no parseable PID, age %ds)\n",
				action, filepath.Base(it.Path), it.AgeSec)
		case "skip-alive":
			fmt.Fprintf(os.Stderr, "kept   live lock %s (PID %d, age %ds)\n",
				filepath.Base(it.Path), it.PID, it.AgeSec)
		case "skip-young":
			fmt.Fprintf(os.Stderr, "kept   young lock %s (age %ds, may still be initialising)\n",
				filepath.Base(it.Path), it.AgeSec)
		case "skip-unreadable":
			fmt.Fprintf(os.Stderr, "kept   unreadable lock %s (no parseable PID, age %ds) — pass --all to remove\n",
				filepath.Base(it.Path), it.AgeSec)
		}
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "(dry-run; would have reclaimed %d lock(s))\n", countAction(items, "remove-dead")+countAction(items, "remove-unreadable"))
	} else {
		fmt.Fprintf(os.Stderr, "reclaimed %d lock(s)\n", removed)
	}
	return nil
}

func countAction(items []reapItem, action string) int {
	n := 0
	for _, it := range items {
		if it.Action == action {
			n++
		}
	}
	return n
}

func init() {
	sessionsReapCmd.Flags().Bool("dry-run", false, "Print what would be reclaimed without removing anything")
	sessionsReapCmd.Flags().Bool("all", false, "Also remove locks whose PID file is empty or unreadable")
}
