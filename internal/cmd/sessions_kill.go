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

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <id>",
	Short: "Kill the process holding a session's lock and remove the lock file",
	Long: `Force-release a session that is stuck behind a live or orphan
crush process. Reads the holder PID from .crush/locks/session-<id>.lock,
forcibly kills it (SIGKILL on POSIX, taskkill /F /T on Windows so the
whole subprocess tree dies), waits for the OS to release the file
handle, then removes the lock file.

Use this when:
- A "crush run --session <id>" reports "session is already in use", but
  you know the real holder is dead (or stuck) and won't release.
- "crush sessions reset --force" cannot proceed because the lock survived.
- A previous run was force-killed (TaskStop / Ctrl+C on a wrapper) and
  left the child crush process orphaned, still holding the lock.

On Windows the kill goes through ` + "`taskkill /F /T /PID`" + ` which
also terminates every child the crush process spawned (typically the
external CLI: claude.cmd → node.exe). The plain os.Process.Kill() goes
through OpenProcess(PROCESS_TERMINATE), which can fail with "Access is
denied" for processes launched under Git Bash or MSYS — taskkill avoids
that whole class of issue.

By default the lock is removed even if the kill failed (process already
gone). Pass --keep-lock to skip the file removal.`,
	Example: `
crush sessions kill pr-42
crush sessions kill pr-42 --keep-lock     # just kill, leave the lock file
crush sessions kill pr-42 --wait 10s      # wait up to 10s for the PID to die
  `,
	Args: cobra.ExactArgs(1),
	RunE: sessionsKillCmdRun,
}

func sessionsKillCmdRun(cmd *cobra.Command, args []string) error {
	id := args[0]
	keepLock, _ := cmd.Flags().GetBool("keep-lock")
	wait, _ := cmd.Flags().GetDuration("wait")
	if wait <= 0 {
		wait = 5 * time.Second
	}

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	lockPath := filepath.Join(cwd, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(id)+".lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no lock file at %s\n", lockPath)
			return nil
		}
		return fmt.Errorf("stat lock: %w", err)
	}

	pid := session.ReadLockPID(lockPath)
	killReport := forceKillHolder(pid, wait)
	fmt.Fprint(os.Stderr, killReport)

	if keepLock {
		fmt.Fprintf(os.Stderr, "lock file kept at %s (age %ds)\n", lockPath, age(info))
		return nil
	}

	if err := removeLockWithRetry(lockPath, wait); err != nil {
		return fmt.Errorf("remove lock %s: %w (the process may still hold the handle — retry in a moment)", lockPath, err)
	}
	fmt.Fprintf(os.Stderr, "removed lock %s\n", lockPath)
	return nil
}

// forceKillHolder kills the PID (no-op for pid<=0) and waits up to `wait`
// for it to actually exit. Returns a human-readable, multi-line report.
// Safe to call when the process is already dead.
func forceKillHolder(pid int, wait time.Duration) string {
	var sb strings.Builder
	if pid <= 0 {
		sb.WriteString("lock has no readable PID; removing file only\n")
		return sb.String()
	}
	if !session.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "PID %d already gone\n", pid)
		return sb.String()
	}
	if err := session.KillProcess(pid); err != nil {
		fmt.Fprintf(&sb, "kill PID %d: %v\n", pid, err)
	} else {
		fmt.Fprintf(&sb, "killed PID %d\n", pid)
	}
	// Poll until dead or wait elapses (taskkill/SIGKILL is async).
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !session.IsProcessAlive(pid) {
			fmt.Fprintf(&sb, "PID %d exited\n", pid)
			return sb.String()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if session.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "warning: PID %d still alive after %s wait\n", pid, wait)
	} else {
		fmt.Fprintf(&sb, "PID %d exited\n", pid)
	}
	return sb.String()
}

// removeLockWithRetry tries to delete the lock file until it succeeds or
// `wait` elapses. On Windows the file handle held by a just-killed
// process can take a moment to release; an immediate Remove returns
// ERROR_SHARING_VIOLATION ("the process cannot access the file because
// it is being used by another process"). Retrying with a small backoff
// covers that window without a hardcoded sleep.
func removeLockWithRetry(path string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		err := os.Remove(path)
		if err == nil {
			return nil
		}
		if os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return lastErr
}

func age(info os.FileInfo) int {
	if info == nil {
		return 0
	}
	return int(time.Since(info.ModTime()).Seconds())
}

// sanitiseSessionIDForFilename mirrors session.sanitiseSessionID (package-private)
// so the lock-file path resolves the same way the lock acquirer wrote it.
func sanitiseSessionIDForFilename(id string) string {
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		`"`, "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	)
	return r.Replace(id)
}

func init() {
	sessionsKillCmd.Flags().Bool("keep-lock", false, "Kill the process but do not delete the lock file")
	sessionsKillCmd.Flags().Duration("wait", 5*time.Second, "How long to wait for the PID to die and the OS to release the lock handle")
}
