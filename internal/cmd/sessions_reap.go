package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsReapCmd = &cobra.Command{
	Use:   "reap",
	Short: "Remove lock files proven (via a real OS lock probe) to have no live holder",
	Long: `Scan .crush/locks/ and remove any lock that a real OS-level lock
probe (actually attempting flock/LockFileEx) proves has no live holder.
Unlike the old PID/mtime heuristic — which could unlink a path out from
under a live OS lock (advisory locks are bound to the inode, not the path,
so the unlink didn't revoke the live holder, it just let a second process
create a fresh inode at the same path and believe it owned the session:
two owners of one session id) — reap now acquires the real lock per entry
and only removes files it could freely acquire.

Use --dry-run to see what would be reclaimed without touching anything.

Note: an empty lock file with no held OS lock is harmless (the next
acquirer reopens and overwrites it; see internal/session/lock.go's
Release), so removing these files is cosmetic cleanup, not a correctness
requirement — reap exists to keep the locks directory tidy for operators.

The legacy --all flag is accepted for backward compatibility but is now a
no-op: removal is decided solely by the OS-lock probe, so a lock with an
unreadable PID is removed if (and only if) the probe proves no live holder,
regardless of --all.`,
	Example: `
crush sessions reap
crush sessions reap --dry-run
crush sessions reap --all      # also nuke locks with unreadable PIDs
  `,
	RunE: sessionsReapCmdRun,
}

type reapItem struct {
	Path   string
	PID    int
	AgeSec int
	Action string // "remove-dead", "remove-unreadable", "skip-alive", "skip-young"
}

func sessionsReapCmdRun(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	// Resolve the data directory the same lightweight way `sessions kill`
	// does (task #219/#224): honor --data-dir first, then the project's
	// configured data_directory, and only fall back to <cwd>/.crush if
	// neither is set. This command is purely filesystem-based (no DB,
	// no provider config), so config.ResolveDataDirectory's pure, local
	// resolution is used instead of pulling in the full setupApp. See
	// task #233 — the same cwd-hardcoding bug class as #219/#224/#231.
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	dataDir, err := config.ResolveDataDirectory(cwd, dataDirFlag)
	if err != nil {
		return fmt.Errorf("failed to resolve data directory: %w", err)
	}

	locksDir := filepath.Join(dataDir, "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "(no locks directory)")
			return nil
		}
		return err
	}

	// A lock younger than one heartbeat may be mid-acquisition; see the
	// skip-young branch below for why probing it is actively dangerous.
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
		sessionID := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "session-"), ".lock")
		pid := session.ReadLockPID(path)
		item := reapItem{Path: path, PID: pid, AgeSec: int(age.Seconds())}

		// Too young to judge — skip without even probing.
		//
		// session.acquireSessionLockFile opens the file and takes the OS lock
		// as two SEPARATE syscalls (internal/session/lock.go: os.OpenFile then
		// tryLockFile). In the window between them the file exists on disk
		// with age ~0 and no lock held, so a probe would acquire successfully
		// and we would classify a session that is starting RIGHT NOW as
		// "provably dead" and unlink its path. That process then locks its own
		// (now unlinked) inode and believes it owns the session, while the
		// next run creates a fresh inode at the same path and also acquires:
		// two owners of one session id — the exact bug this command exists to
		// prevent, reintroduced by the cleanup itself.
		//
		// Young locks are also the worst case for the probe-induced contention
		// documented on lockHolderProvablyDead: a lock seconds old is far more
		// likely to belong to a run that is starting than to the crash backlog
		// reap is for. Skipping them costs nothing — they are not the backlog.
		if age < heartbeatThreshold {
			item.Action = "skip-young"
			items = append(items, item)
			continue
		}

		// The ONLY grounds for removing a stable per-session lock file is
		// proving at the OS level that no live process holds it — by actually
		// acquiring the real OS lock (flock/LockFileEx). PID liveness and
		// mtime age are reporting only here: a stale/dead RECORDED pid is not
		// proof the holder is gone (the OS may have recycled that PID number
		// for an unrelated process, or a live holder may be running with a
		// cleared/stale PID after a partial write). Unlinking a path out from
		// under a live holder does not revoke its OS lock (advisory locks are
		// bound to the inode, not the path); it just lets a second process
		// create a fresh inode at the same path and believe it owns the
		// session — two owners of one session id. See internal/session/lock.go
		// and lockHolderProvablyDead (sessions.go) for the full rationale.
		lk, probeErr := session.TryAcquireSessionLock(dataDir, sessionID)
		switch {
		case probeErr == nil:
			// Nobody holds the OS lock — provably dead. Release our probe and
			// mark for removal. The narrow release→Remove TOCTOU this leaves
			// is the same accepted one documented for `sessions locks --prune`
			// (reap is an explicit, operator-invoked sweep).
			_ = lk.Release()
			item.Action = "remove-dead"
		default:
			var busyErr *session.SessionLockBusyError
			if errors.As(probeErr, &busyErr) {
				item.Action = "skip-alive"
			} else {
				// Unknown probe failure — fail closed; never unlink on an
				// uncertain state.
				item.Action = "skip-probe-error"
			}
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
				if err := os.Remove(it.Path); err == nil || os.IsNotExist(err) {
					action = "removed"
					removed++
				} else {
					action = "failed to remove (" + err.Error() + ")"
				}
			}
			fmt.Fprintf(os.Stderr, "%s orphan lock %s (holder provably dead via OS lock, age %ds)\n",
				action, filepath.Base(it.Path), it.AgeSec)
		case "skip-alive":
			fmt.Fprintf(os.Stderr, "kept   live lock %s (OS lock held, age %ds)\n",
				filepath.Base(it.Path), it.AgeSec)
		case "skip-probe-error":
			fmt.Fprintf(os.Stderr, "kept   lock %s (probe inconclusive; left untouched, age %ds)\n",
				filepath.Base(it.Path), it.AgeSec)
		case "skip-young":
			fmt.Fprintf(os.Stderr, "kept   young lock %s (age %ds, may still be mid-acquisition — not probed)\n",
				filepath.Base(it.Path), it.AgeSec)
		}
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "(dry-run; would have reclaimed %d lock(s))\n", countAction(items, "remove-dead"))
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
	sessionsReapCmd.Flags().Bool("all", false, "Legacy no-op: removal is now decided solely by the real OS-lock probe, so unreadable-PID locks are reaped iff proven uncontended regardless of this flag")
}
