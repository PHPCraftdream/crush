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

The lock FILE is only removed once the holder is PROVEN dead (either the
OS lock was acquirable — nobody holds it — or the kill was issued and the
PID was observed to exit) AND a fresh OS-lock probe still succeeds right
before the unlink. If the kill fails, the PID is still alive after the
wait, or the lock state is unknown, the file is LEFT IN PLACE — an empty
lock file with no held OS lock is harmless (the next acquirer simply
reopens and overwrites it; see internal/session/lock.go), and removing a
path out from under a live OS lock is exactly the two-owners bug this
command must never trigger. Pass --keep-lock to skip the file removal
entirely even on a confirmed kill.`,
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
	// set. config.ResolveDataDirectory is pure, local, filesystem-only
	// config resolution (no network fetch, no DB connection, no config
	// persistence side effects), so it stays safe to use from a rescue
	// command that must keep working even when the network/DB/app is
	// stuck.
	//
	// Always use the resolved value, never the raw --data-dir flag
	// string directly: a RELATIVE --data-dir must be resolved against
	// --cwd (like setupApp/sessions.go's reset --force does), not
	// against the process's actual cwd, which can differ. See task #224
	// finding 1.
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	dataDir, err := config.ResolveDataDirectory(cwd, dataDirFlag)
	if err != nil {
		return fmt.Errorf("failed to resolve data directory: %w", err)
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
	kr := probeThenKillHolder(dataDir, id, pid, wait)
	fmt.Fprint(os.Stderr, kr.Report)

	if keepLock {
		fmt.Fprintf(os.Stderr, "lock file kept at %s (age %ds)\n", lockPath, age(info))
		return nil
	}

	// The lock file is removed ONLY once the holder is proven dead. Removing
	// it while a holder may still be alive (kill failed, the PID survived the
	// wait, or the probe was inconclusive) risks unlinking a live OS lock's
	// path — on POSIX flock is bound to the inode so the unlink doesn't
	// revoke the live holder, it just lets a second process create a fresh
	// inode at the same path and believe it owns the session: two owners of
	// one session id. An empty lock file left behind is harmless (the next
	// acquirer reopens and overwrites it), so fail closed here.
	if !kr.ConfirmedDead {
		fmt.Fprintf(os.Stderr, "warning: holder not confirmed dead; leaving lock file %s in place\n", lockPath)
		return fmt.Errorf("could not confirm the lock holder is dead; lock file left in place at %s", lockPath)
	}

	// If the probe above took the "already dead" branch (probeThenKillHolder's
	// own TryAcquireSessionLock+Release), that Release() returns as soon as
	// its synchronous unlock/close finish (session.SessionLock's Mechanism-1
	// fix) — its background metadata-cleanup goroutine can still briefly hold
	// the OS lock (re-acquired for a Truncate/Sync) afterward. Without
	// waiting for it to settle, the SECOND probe immediately below could
	// spuriously observe SessionLockBusyError against our OWN prior release,
	// not a genuine new holder, and wrongly refuse to remove an already-dead
	// lock. This is a bounded wait on a background goroutine finishing
	// syscalls that normally take microseconds — not a new unbounded block.
	waitForLockMetadataSettled(lockPath, wait)

	// Final OS-level proof immediately before the destructive remove: a NEW
	// owner may have grabbed the session between the kill above and now. If
	// the real OS lock is currently held (or its state is unknown), leave the
	// file rather than risk unlinking a live holder's path.
	if !lockHolderProvablyDead(dataDir, id) {
		fmt.Fprintf(os.Stderr, "warning: lock is currently held (or its state is unknown); leaving lock file %s in place\n", lockPath)
		return nil
	}
	if err := removeLockWithRetry(lockPath, wait); err != nil {
		return fmt.Errorf("remove lock %s: %w (the process may still hold the handle — retry in a moment)", lockPath, err)
	}
	fmt.Fprintf(os.Stderr, "removed lock %s\n", lockPath)
	return nil
}

// waitForLockMetadataSettled polls lockPath's recorded PID until it reads 0
// (cleared) or budget elapses. Used between two back-to-back probes
// (TryAcquireSessionLock+Release cycles) against the same lock path: a probe's
// own Release() returns as soon as its synchronous unlock/close finish (see
// session.SessionLock.Release's Mechanism-1 doc comment), while its
// background metadata-cleanup goroutine can still briefly hold the OS lock
// (re-acquired for its own Truncate/Sync) afterward. A second probe launched
// immediately can otherwise race that goroutine and observe
// SessionLockBusyError against the FIRST probe's own in-flight cleanup,
// mistaking it for a genuine new holder. Bounded by budget (capped at 2s,
// since cleanup normally finishes in microseconds — this only ever waits
// meaningfully under genuine filesystem contention, which the caller's own
// lockHolderProvablyDead re-check handles safely either way). Best-effort:
// never returns an error, since a caller-visible timeout here would be worse
// than falling through to the caller's own authoritative probe.
func waitForLockMetadataSettled(lockPath string, budget time.Duration) {
	if budget <= 0 || budget > 2*time.Second {
		budget = 2 * time.Second
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if session.ReadLockPID(lockPath) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holderState constants describe what probeThenKillHolder concluded and did.
// Only the decision-relevant distinction matters to callers: ConfirmedDead
// (true only when the holder is proven gone at the OS level) gates every
// destructive follow-up. The State label is for operator-facing reports.
const (
	// holderAlreadyDead: the probe itself acquired the OS lock — nobody holds
	// it — so nothing was killed.
	holderAlreadyDead = "already-dead"
	// holderKilled: the probe found live contention and forceKillHolder
	// observed the PID exit within the wait budget.
	holderKilled = "killed"
	// holderStillAlive: a kill was issued but the PID was still alive when the
	// wait budget elapsed. NOT confirmed dead.
	holderStillAlive = "still-alive"
	// holderProbeError: TryAcquireSessionLock returned a non-busy error
	// (permission denied, IO error, ...). The lock state is genuinely unknown,
	// so nothing was killed (fail closed).
	holderProbeError = "probe-error"
	// holderUnidentified: the probe proved a live holder exists (busy error)
	// but neither the sidecar nor the lock file yielded a usable PID, so there
	// is nothing to kill and nothing can be confirmed dead. Distinct from
	// holderAlreadyDead precisely because a holder demonstrably DOES exist.
	holderUnidentified = "unidentified-holder"
)

// killResult is the structured outcome of probeThenKillHolder. ConfirmedDead
// is true ONLY when the holder has been proven gone at the OS level — either
// the probe acquired the lock (nobody holds it) or forceKillHolder observed
// the recorded PID exit. Callers MUST NOT perform any destructive follow-up
// (removing the lock file, wiping a session) unless ConfirmedDead is true; a
// false here means the holder might still be alive and must be left alone.
type killResult struct {
	State         string
	KilledPID     int
	ConfirmedDead bool
	Report        string
}

// probeThenKillHolder decides, via a real OS-level lock attempt, whether the
// PID recorded in the lock file is still a genuine live holder before handing
// off to forceKillHolder.
//
// Why: the lock file's PID is metadata, not proof. A holder that exited
// cleanly runs Release(), which clears that metadata (see
// session.SessionLock.Release), but an OLD lock file predating that fix, or
// one whose Release() never ran (process crashed and the file was left
// mid-write, or something wrote the file directly, as tests sometimes do)
// can still carry a stale PID. If the OS has since recycled that PID number
// for a totally unrelated process — routine on a busy CI/dev box — blindly
// kill(pid)-ing it would take out an innocent process.
//
// The only authoritative signal for "is anyone actually still holding this
// lock" is attempting to take the real OS lock ourselves:
//   - If TryAcquireSessionLock SUCCEEDS, nobody holds the lock right now,
//     full stop (see the package doc on SessionLock) — the recorded PID, if
//     any, is stale/dead/reused. We must NOT touch that PID. Release our
//     probe lock immediately and report ConfirmedDead (the holder is gone).
//   - If it reports *session.SessionLockBusyError, someone genuinely holds
//     the OS lock right now — the classic case this command exists for —
//     proceed to forceKillHolder exactly as before.
//   - Any OTHER error (permission denied, IO error, etc.) is NOT proof of
//     either state. FAIL CLOSED: return holderProbeError with
//     ConfirmedDead=false and do NOT kill the recorded PID. The previous
//     behavior fell back to unconditionally killing whatever PID was
//     recorded on any unidentified probe failure — exactly the stale/reused
//     PID danger the busy/acquire branches exist to avoid, just reached via a
//     different path. An unrelated IO hiccup must not turn this command into
//     "kill an arbitrary PID number off disk".
//
// What this does NOT solve: a residual PID-reuse TOCTOU window between "probe
// proved contention" and "the PID is actually killed". The busy-error branch
// proves someone holds the lock at the moment TryAcquireSessionLock ran, but
// forceKillHolder then makes its own separate IsProcessAlive + KillProcess
// syscalls at a later moment. In principle the OS could recycle that exact PID
// for an unrelated process in the gap. Closing this fully would require
// retaining an OS-level handle to the process at probe time and killing
// through that handle (Windows HANDLE could; POSIX needs pidfd). Given this is
// a manual rescue tool an operator runs deliberately, that structural fix is
// not implemented; the window is accepted as a narrow, known limitation.
func probeThenKillHolder(dataDir, sessionID string, pid int, wait time.Duration) killResult {
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err == nil {
		// Proved nobody holds the OS lock. Whatever PID is recorded is stale.
		_ = lk.Release()
		var sb strings.Builder
		if pid > 0 {
			fmt.Fprintf(&sb, "lock probe acquired the lock: PID %d is stale (holder already gone); not killing anything\n", pid)
		} else {
			sb.WriteString("lock probe acquired the lock: no live holder; nothing to kill\n")
		}
		return killResult{State: holderAlreadyDead, ConfirmedDead: true, Report: sb.String()}
	}
	var busyErr *session.SessionLockBusyError
	if errors.As(err, &busyErr) {
		// A real process holds the OS lock right now. Prefer the PID the probe
		// itself identified (it reads the never-locked sidecar) but fall back
		// to the caller-supplied one for safety.
		livePID := busyErr.HolderPID
		if livePID <= 0 {
			livePID = pid
		}
		return forceKillHolder(livePID, wait)
	}
	// Unidentified probe failure — NOT proof of either state. Fail closed:
	// never kill the recorded PID on an uncertain lock state.
	return killResult{
		State:  holderProbeError,
		Report: fmt.Sprintf("lock probe failed with an unidentified error (not proof of a live holder): %v\nrefusing to kill recorded PID %d — the lock state is unknown\n", err, pid),
	}
}

// forceKillHolder kills the PID (no-op for pid<=0) and waits up to `wait` for
// it to actually exit. Returns a structured killResult; ConfirmedDead is true
// only when the PID is observed to be gone. Safe to call when the process is
// already dead.
func forceKillHolder(pid int, wait time.Duration) killResult {
	var sb strings.Builder
	if pid <= 0 {
		// This function is ONLY ever reached from the busy branch of
		// probeThenKillHolder / acquireSessionLockForReset — i.e. after the OS
		// has just proven that a live holder exists. So "no usable PID" here
		// does NOT mean "nobody is holding the lock"; it means a holder
		// demonstrably exists and we cannot identify it. Reporting
		// ConfirmedDead here would be a claim the code cannot support, and
		// every destructive follow-up is gated on exactly that field.
		sb.WriteString("a live holder holds the OS lock but no usable PID could be read " +
			"(empty/garbage lock file and no sidecar); nothing can be killed and nothing is confirmed dead\n")
		return killResult{State: holderUnidentified, ConfirmedDead: false, Report: sb.String()}
	}
	if !session.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "PID %d already gone\n", pid)
		return killResult{State: holderAlreadyDead, KilledPID: pid, ConfirmedDead: true, Report: sb.String()}
	}
	if err := session.KillProcess(pid); err != nil {
		// KillProcess itself errored. The PID may yet exit asynchronously
		// (kill is async on Windows), so still poll the wait budget — but if
		// it is alive at the end, ConfirmedDead stays false.
		fmt.Fprintf(&sb, "kill PID %d: %v\n", pid, err)
	} else {
		fmt.Fprintf(&sb, "killed PID %d\n", pid)
	}
	// Poll until dead or wait elapses (taskkill/SIGKILL is async).
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !session.IsProcessAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if session.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "warning: PID %d still alive after %s wait\n", pid, wait)
		return killResult{State: holderStillAlive, KilledPID: pid, ConfirmedDead: false, Report: sb.String()}
	}
	fmt.Fprintf(&sb, "PID %d exited\n", pid)
	return killResult{State: holderKilled, KilledPID: pid, ConfirmedDead: true, Report: sb.String()}
}

// acquireSessionLockForReset acquires the real OS session lock, killing the
// current holder first if one is alive (and confirmable). It returns the HELD
// lock (caller MUST Release) so no concurrent crush process can grab the
// session during a critical section that follows — in particular `sessions
// reset --force`'s DB wipe, which must not race a fresh `crush run --session
// <id>` that recreates the lock at the same path and starts writing.
//
// This is the only place in the sessions CLI that holds the OS lock across a
// non-removal operation, and it is correct on both platforms precisely BECAUSE
// it does not unlink the path while holding it: holding a lock to serialize
// access is exactly what OS locks are for. (Unlinking while holding is the
// Windows-fragile case the lockHolderProvablyDead doc comment rejects; this
// path avoids it entirely by never removing the file.)
//
// Fails closed — returns a non-nil error and a nil lock whenever the holder
// could not be proven dead (kill failed, PID still alive, or the probe was
// inconclusive). It NEVER falls back to "act without the lock": a reset that
// proceeds without holding the lock would race a live/new holder against the
// DB wipe, which is strictly worse than refusing to reset.
func acquireSessionLockForReset(dataDir, sessionID string, pid int, wait time.Duration) (*session.SessionLock, killResult, error) {
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err == nil {
		return lk, killResult{State: holderAlreadyDead, ConfirmedDead: true, Report: "lock acquired; no live holder\n"}, nil
	}
	var busyErr *session.SessionLockBusyError
	if !errors.As(err, &busyErr) {
		// Unknown probe failure — fail closed; never act without the lock.
		return nil, killResult{State: holderProbeError, Report: fmt.Sprintf("lock probe failed (state unknown): %v\n", err)},
			fmt.Errorf("could not determine lock state for session %s: %w", sessionID, err)
	}
	// A live process holds the lock. Kill it, wait for confirmed death, then
	// acquire so we hold it through the critical section that follows.
	livePID := busyErr.HolderPID
	if livePID <= 0 {
		livePID = pid
	}
	kr := forceKillHolder(livePID, wait)
	if !kr.ConfirmedDead {
		return nil, kr, fmt.Errorf("could not confirm the lock holder (PID %d) is dead; session left untouched", livePID)
	}
	lk, err = session.TryAcquireSessionLock(dataDir, sessionID)
	if err == nil {
		return lk, kr, nil
	}
	if errors.As(err, &busyErr) {
		return nil, kr, fmt.Errorf("session %s is still locked after killing PID %d; another process may have acquired it", sessionID, livePID)
	}
	return nil, kr, fmt.Errorf("could not re-acquire lock for session %s after kill: %w", sessionID, err)
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
