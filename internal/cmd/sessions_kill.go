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

	// Resolve the data directory the same lightweight way `stats` does:
	// honor --data-dir first, then the project's configured
	// data_directory, and only fall back to <cwd>/.crush if neither is
	// set. config.Init is pure config resolution (no DB connection), so
	// it stays safe to use from a rescue command that must keep working
	// even when the DB/app is stuck.
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	debug, _ := cmd.Flags().GetBool("debug")
	cfg, err := config.Init(cwd, dataDirFlag, debug)
	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}
	dataDir := dataDirFlag
	if dataDir == "" {
		dataDir = cfg.Config().Options.DataDirectory
	}

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(id)+".lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no lock file at %s\n", lockPath)
			return nil
		}
		return fmt.Errorf("stat lock: %w", err)
	}

	pid := session.ReadLockPID(lockPath)
	killReport := probeThenKillHolder(dataDir, id, pid, wait)
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

// probeThenKillHolder decides, via a real OS-level lock attempt, whether
// the PID recorded in the lock file is still a genuine live holder before
// handing off to forceKillHolder.
//
// Why: the lock file's PID is metadata, not proof. A holder that exited
// cleanly runs Release(), which now clears that metadata (see
// session.SessionLock.Release), but an OLD lock file predating that fix,
// or one whose Release() never ran (process crashed and the file was left
// mid-write, or something wrote the file directly, as tests sometimes do)
// can still carry a stale PID. If the OS has since recycled that PID
// number for a totally unrelated process — routine on a busy CI/dev box —
// blindly kill(pid)-ing it would take out an innocent process.
//
// The only authoritative signal for "is anyone actually still holding
// this lock" is attempting to take the real OS lock ourselves:
//   - If TryAcquireSessionLock SUCCEEDS, nobody holds the lock right now,
//     full stop (see the package doc on SessionLock) — the recorded PID,
//     if any, is stale/dead/reused. We must NOT touch that PID. Release
//     our probe lock immediately (we don't want to hold the session) and
//     report the lock as dead so the caller can proceed straight to
//     removing the lock file.
//   - If it reports *session.SessionLockBusyError, someone genuinely
//     holds the OS lock right now — the classic case this command exists
//     for (a stuck/orphaned live crush process) — proceed to
//     forceKillHolder exactly as before.
//   - Any other error (permission denied, IO error, etc.) is NOT proof of
//     either state; fall back to the previous unconditional behavior
//     (kill whatever PID was recorded) rather than silently doing nothing
//     — this preserves existing behavior for callers/tests that never
//     exercised the probe path and avoids turning an unrelated IO hiccup
//     into "sessions kill now refuses to do anything".
//
// What this does NOT solve: a residual PID-reuse TOCTOU (time-of-check-to-
// time-of-use) window between "probe proved contention" and "the PID is
// actually killed". The busy-error branch above proves someone holds the
// lock at the moment TryAcquireSessionLock ran, but forceKillHolder then
// makes its own separate IsProcessAlive + KillProcess syscalls at a later
// moment — not one atomic operation with the probe, and not atomic with
// each other either. In principle the OS could recycle that exact PID for
// an unrelated process in the gap between the probe and the kill (or even
// between forceKillHolder's own liveness check and its kill call, however
// small that gap is). Closing this fully would require retaining an
// OS-level handle to the process at probe time and killing through that
// handle rather than by PID number again later — Windows' HANDLE model
// could support this, POSIX plain PIDs fundamentally can't without extra
// platform-specific plumbing (e.g. Linux pidfd), and this is a
// cross-platform CLI. Given this command's scope — a manual rescue tool an
// operator runs deliberately, not an unattended automated killer — that
// structural fix is not implemented; the window is accepted as a narrow,
// known limitation rather than something silently assumed to be airtight.
func probeThenKillHolder(dataDir, sessionID string, pid int, wait time.Duration) string {
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err == nil {
		// We just proved nobody holds the OS lock. Whatever PID is
		// recorded in the file is stale — do not kill it.
		_ = lk.Release()
		var sb strings.Builder
		if pid > 0 {
			fmt.Fprintf(&sb, "lock probe acquired the lock: PID %d is stale (holder already gone); not killing anything\n", pid)
		} else {
			sb.WriteString("lock probe acquired the lock: no live holder; nothing to kill\n")
		}
		return sb.String()
	}
	var busyErr *session.SessionLockBusyError
	if errors.As(err, &busyErr) {
		// A real process holds the OS lock right now. Prefer the PID the
		// probe itself identified (it reads the never-locked sidecar,
		// see readLockFile's doc comment) but fall back to the
		// caller-supplied one for safety.
		livePID := busyErr.HolderPID
		if livePID <= 0 {
			livePID = pid
		}
		return forceKillHolder(livePID, wait)
	}
	// Unidentified probe failure — not proof of either state. Preserve
	// prior behavior rather than refusing to act.
	return forceKillHolder(pid, wait)
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
