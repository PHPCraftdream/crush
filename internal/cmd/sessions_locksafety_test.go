package cmd

// Regression coverage for the #269 P0-5 release blocker: cleanup of stable
// per-session lock files could unlink a path a LIVE OS lock was held on. On
// POSIX, unlinking a path does NOT release flock on the old inode, so a second
// process could create a fresh inode at the same path and believe it owned the
// session while the original holder kept running against the old inode — two
// owners of one session id, both writing the same session DB.
//
// The fix direction (from release review) this file enforces:
//   - `sessions locks` is READ-ONLY by default (no auto-delete unless --prune).
//   - `sessions reap` decides by a real OS-lock probe, never by PID/mtime alone.
//   - `sessions kill` FAILS CLOSED on an unknown probe error (never falls back
//     to killing the recorded PID) and only removes the file once the holder is
//     confirmed dead.
//   - `sessions reset --force` HOLDS the OS lock across the DB reset and does
//     NOT remove the lock file (an empty lock file is harmless).
//
// Each test here is written to compile against BOTH the old and new production
// code (it drives the real command RunE / helpers that exist in both), so the
// revert-verification prescribed by the review actually runs the test against
// the buggy code and fails on the intended assertion rather than a compile
// error.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestSessionsLocks_ReadOnlyByDefault_StaleLockNotPruned enforces place 2:
// `sessions locks` with NO --prune must leave even a provably-dead, stale lock
// file untouched. The old behavior auto-deleted such entries on every
// invocation — an unconditional, non-opt-in destructive default that opened
// the post-probe TOCTOU window on every run. Reverting the fix (dropping the
// `prune &&` gate) makes this test fail: the lock is removed.
func TestSessionsLocks_ReadOnlyByDefault_StaleLockNotPruned(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "readonly-default-data")
	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("prune", "false")) // the whole point: default is read-only
	sessionsLocksCmd.SetContext(context.Background())

	const sessionID = "readonly-default-id"
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sessionID+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))

	// Backdate well past the 60s auto-delete threshold so the old default
	// behavior would have targeted this entry.
	old := time.Now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	// Sanity: the lock IS provably dead (no live OS holder), so the ONLY thing
	// standing between this file and removal is the --prune gate. This probe
	// call freshens mtime + clears content as a side effect, so restore both
	// afterward or the real run below would not see a stale-by-mtime entry.
	require.True(t, lockHolderProvablyDead(dataDir, sessionID),
		"precondition: lock must be provably dead for the --prune gate to be the only barrier")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))
	require.NoError(t, os.Chtimes(lockPath, old, old))

	stdout, stderr := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil))
	})
	t.Logf("sessions locks stdout:\n%s", stdout)
	t.Logf("sessions locks stderr:\n%s", stderr)

	require.NotContains(t, stderr, "removed stale lock",
		"a read-only `sessions locks` (no --prune) must never remove a lock")
	require.FileExists(t, lockPath,
		"a stale-but-provably-dead lock must survive a read-only `sessions locks` invocation")
}

// TestSessionsReap_LiveOSLockStaleRecordedPID_NotRemoved enforces place 3: reap
// must decide removal by a REAL OS-lock probe, never by the recorded PID's
// liveness. Here a genuine second process holds the OS lock, but the lock's
// sidecar names a guaranteed-dead PID (999999) — exactly the PID-reuse /
// stale-PID shape. Old reap (IsProcessAlive(recordedPID) heuristic) would
// classify this as "remove-dead" and unlink the path out from under the live
// holder; new reap acquires the real lock, sees contention, and skips it.
//
// The assertion is on reap's DECISION (the report), which is the actual bug
// and is cross-platform: on POSIX the old code's unlink additionally succeeds
// (file gone), on Windows it fails with a sharing violation but reap still
// REPORTS "orphan lock" — either way the report assertion fails on old code.
func TestSessionsReap_LiveOSLockStaleRecordedPID_NotRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "reap-oslock-data")
	ensureRootFlagStandIns(sessionsReapCmd, dataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())

	const sessionID = "reap-oslock-live-holder"
	// A REAL second process holds the OS lock on this session.
	holder := spawnKillTestLockHolder(t, dataDir, sessionID)
	defer holder.stop()
	require.True(t, session.IsProcessAlive(holder.pid))

	lockPath := filepath.Join(dataDir, "locks", "session-"+sessionID+".lock")
	require.FileExists(t, lockPath)

	// Rewrite the never-locked PID sidecar to name a guaranteed-dead PID, so a
	// PID-liveness heuristic (the old decision basis) sees a dead holder while a
	// live process still holds the real OS lock. The sidecar is never locked, so
	// writing it never disturbs the live holder.
	require.NoError(t, os.WriteFile(lockPath+".pid", []byte(fmt.Sprintf("%d\n", 999999)), 0o644))

	// Age the lock past reap's heartbeatThreshold so it reaches the OS-lock
	// probe at all — otherwise it is skipped as "young" (mid-acquisition) and
	// this test would assert nothing about the probe. Safe to backdate under a
	// live holder: the heartbeat only touches mtime when there was activity
	// (see session.heartbeat's active.Swap gate, task #222), and this holder
	// is idle.
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	stderr := captureStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})
	t.Logf("sessions reap stderr:\n%s", stderr)

	require.Contains(t, stderr, "kept   live lock",
		"a lock whose OS lock is held must be skipped, even when its recorded PID looks dead")
	require.NotContains(t, stderr, "orphan lock",
		"reap must never decide to remove a lock whose OS lock is currently held")
	require.True(t, session.IsProcessAlive(holder.pid), "reap must never disturb a live holder")
}

// TestProbeThenKillHolder_UnknownProbeError_FailsClosed enforces place 5: when
// TryAcquireSessionLock returns a non-busy error (permission denied, IO error,
// unwritable locks dir, ...), the lock state is genuinely UNKNOWN. Old code
// fell through to forceKillHolder(recordedPID) on ANY such error — killing an
// arbitrary PID off disk, the exact PID-reuse danger the busy/acquire branches
// exist to avoid, just reached via a different path. The fix fails closed: do
// nothing, report state unknown.
//
// The unknown-error condition is forced by making dataDir a regular file, so
// TryAcquireSessionLock's os.MkdirAll(<dataDir>/locks) fails with a non-busy
// error. A live child process stands in for the "unrelated process that
// recycled the recorded PID" — old code kills it; the fix must not.
func TestProbeThenKillHolder_UnknownProbeError_FailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	tmp := t.TempDir()

	// dataDir is a regular FILE, not a directory: MkdirAll(<file>/locks) fails
	// with a non-busy error -> the unidentified-probe-failure branch.
	dataDir := filepath.Join(tmp, "not-a-dir")
	require.NoError(t, os.WriteFile(dataDir, []byte("x"), 0o644))

	// A live child process = the PID-reuse victim old code would kill.
	var child *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		child = exec.CommandContext(context.Background(), "cmd.exe", "/c", "ping", "-n", "30", "127.0.0.1")
	default:
		child = exec.CommandContext(context.Background(), "sleep", "30")
	}
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		if child.Process != nil {
			_ = child.Process.Kill()
		}
		_, _ = child.Process.Wait()
	})
	require.True(t, session.IsProcessAlive(child.Process.Pid), "precondition: child must start alive")

	kr := probeThenKillHolder(dataDir, "unknown-probe-id", child.Process.Pid, 2*time.Second)
	t.Logf("report: %s", kr.Report)

	require.True(t, session.IsProcessAlive(child.Process.Pid),
		"an unidentified probe failure must NOT kill the recorded PID (PID-reuse danger)")
	require.False(t, kr.ConfirmedDead, "unknown probe error -> state unknown, must NOT be reported confirmed dead")
	require.Equal(t, holderProbeError, kr.State, "unknown probe error -> state must be probe-error")
}

// TestSessionsReset_ForceLeavesLockFileInPlace enforces place 6: reset --force
// now acquires and HOLDS the OS lock across the DB reset, then releases — it
// deliberately does NOT remove the lock file (an empty lock file with no held
// OS lock is harmless; the next acquirer reopens and overwrites it). Old code
// unlinked the file, so this assertion fails on the old behavior.
func TestSessionsReset_ForceLeavesLockFileInPlace(t *testing.T) {
	a, cwd := isolatedResetEnv(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-leaves-lock", "regression title")
	require.NoError(t, err)

	// Seed a bare lock file (stale PID, no live OS lock) exactly as
	// TestSessionsReset_ForceDoesNotKillStalePID does.
	lockDir := filepath.Join(cwd, ".crush", "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	require.True(t, session.IsProcessAlive(os.Getpid()))
	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "true"))
	stderr := captureStderr(t, func() {
		require.NoError(t, sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID}))
	})
	t.Logf("reset --force stderr:\n%s", stderr)

	require.FileExists(t, lockPath,
		"reset --force must leave the lock file in place (held+released), not unlink it")
	require.True(t, session.IsProcessAlive(os.Getpid()),
		"reset --force must never kill the calling test process via a stale lock-file PID")
}

// TestSessionsReset_ForceHoldsLockDuringDBReset enforces the HOLD property of
// place 6 directly: while reset --force has acquired the session lock for its
// critical section, a concurrent TryAcquireSessionLock on the same session
// must report contention (the lock is actually held). This is the mutual
// exclusion that prevents a fresh `crush run --session <id>` from racing the
// DB wipe. Exercises acquireSessionLockForReset (the helper reset --force now
// uses) rather than the full RunE, since the hold window is the property under
// test and is most cleanly observed against the returned held lock.
func TestSessionsReset_ForceHoldsLockDuringDBReset(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "reset-hold-data")
	const sessionID = "reset-hold-id"

	// No holder: acquire should succeed immediately and return a HELD lock.
	lk, kr, err := acquireSessionLockForReset(dataDir, sessionID, 0, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, holderAlreadyDead, kr.State)
	require.NotNil(t, lk, "must return a held lock")

	// While we hold it, a second acquirer must see contention — proving the
	// lock is genuinely held across the critical section, not just acquired
	// and immediately released. Capture the returned lock (and defer-release
	// it) so a reverted/buggy run that SUCCEEDS here does not leak a held lock
	// and block the temp-dir cleanup.
	contendedLK, err2 := session.TryAcquireSessionLock(dataDir, sessionID)
	if contendedLK != nil {
		defer contendedLK.Release()
	}
	require.Error(t, err2, "a second acquirer must be blocked while reset --force holds the lock")
	var busyErr *session.SessionLockBusyError
	require.ErrorAs(t, err2, &busyErr, "contention must be reported as a busy error, not some other failure")

	require.NoError(t, lk.Release())

	// After release, the session is free again (the empty lock file remains,
	// harmless — but acquisition must succeed).
	lk2, err3 := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err3, "after reset --force releases, the session must be acquirable again")
	_ = lk2.Release()
}

// TestSessionsReap_YoungLockNotProbedOrRemoved covers a defect introduced by
// the first pass at this task and caught in review, not by the test suite.
//
// That pass replaced reap's PID/mtime heuristic with a real OS-lock probe —
// correct — but also deleted the "skip locks younger than one heartbeat"
// guard, on the reasoning that the probe subsumes it. It does not.
//
// session.acquireSessionLockFile opens the lock file and takes the OS lock as
// two SEPARATE syscalls (internal/session/lock.go: os.OpenFile, then
// tryLockFile). In the window between them the file exists on disk, aged ~0,
// with NO lock held. A probe run in that window ACQUIRES successfully, so reap
// concludes "holder provably dead" and unlinks the path of a session that is
// starting right now. That process then locks its own — now unlinked — inode
// and believes it owns the session, while the next run creates a fresh inode
// at the same path and also acquires: two owners of one session id, produced
// by the very command that exists to prevent them.
//
// A freshly created, unlocked lock file reproduces exactly the state of that
// window. Without the guard, reap removes it.
func TestSessionsReap_YoungLockNotProbedOrRemoved(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "reap-young-data")
	ensureRootFlagStandIns(sessionsReapCmd, dataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())

	const sessionID = "reap-young-mid-acquisition"
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")

	// The mid-acquisition state: the file exists (O_CREATE already ran) but no
	// OS lock is held yet (tryLockFile has not run). mtime is "now", so the
	// file is young. Deliberately NOT backdated — youth is the whole point.
	require.NoError(t, os.WriteFile(lockPath, nil, 0o644))

	stderr := captureStderr(t, func() {
		require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil))
	})
	t.Logf("sessions reap stderr:\n%s", stderr)

	require.FileExists(t, lockPath,
		"reap must not unlink a lock file young enough to be mid-acquisition — the acquirer would then hold an "+
			"unlinked inode while the next run creates a fresh one at the same path: two owners of one session id")
	require.NotContains(t, stderr, "orphan lock",
		"a young lock must not even be classified as an orphan; it was never probed")
	require.Contains(t, stderr, "kept   young lock",
		"reap must report explicitly that it skipped a young lock rather than silently doing nothing")
}
