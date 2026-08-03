package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureStdout runs f while capturing os.Stdout output. Mirrors
// claude_init_test.go's captureStderr / models_use_test.go's inline
// stdout-pipe pattern, factored out here since sessionsLocksCmdRun writes
// its table (and --json output) to os.Stdout, not os.Stderr.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	f()
	_ = w.Close()
	<-done
	return buf.String()
}

func TestSessionsLocks_CreateLockFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create locks directory
	locksDir := filepath.Join(tmpDir, ".crush", "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	// Create a lock file
	lockFile := filepath.Join(locksDir, "session-test-id-1.lock")
	require.NoError(t, os.WriteFile(lockFile, []byte("12345\n"), 0o644))

	// Verify it exists
	require.FileExists(t, lockFile)

	content, err := os.ReadFile(lockFile)
	require.NoError(t, err)
	require.Contains(t, string(content), "12345")
}

func TestSessionsLocks_MultipleFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	locksDir := filepath.Join(tmpDir, ".crush", "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	// Create multiple lock files
	for i := 1; i <= 3; i++ {
		lockFile := filepath.Join(locksDir, "session-id-"+string(rune(i)+48)+".lock")
		require.NoError(t, os.WriteFile(lockFile, []byte("1000"+string(rune(i)+48)), 0o644))
	}

	// Verify all files exist
	entries, err := os.ReadDir(locksDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
}

func TestSessionsLocks_ParseFilename(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	locksDir := filepath.Join(tmpDir, ".crush", "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	lockFile := filepath.Join(locksDir, "session-abc-123.lock")
	require.NoError(t, os.WriteFile(lockFile, []byte("5678"), 0o644))

	// Parse filename
	filename := "session-abc-123.lock"
	sessionID := filename[8 : len(filename)-5] // Remove "session-" prefix and ".lock" suffix
	require.Equal(t, "abc-123", sessionID)
}

// TestLockHolderProvablyDead_StaleMtimeButLiveHolder_NotDeleted is the
// regression test for task #222's PID-gating hardening: `sessions locks`
// used to auto-delete any lock file whose mtime was older than 60s,
// justified by "heartbeat would have touched the file every 10s if the
// holder were alive." Task #214/#222 gated that heartbeat's mtime-touch on
// real RecordActivity() calls, so a genuinely healthy session blocked on a
// single long-running tool call can now look mtime-stale for far longer
// than 60s. This proves lockHolderProvablyDead refuses to call a lock
// "dead" — i.e. safe to auto-delete — when a REAL process still holds the
// OS lock, even though its mtime has been artificially backdated to look
// stale. Mirrors sessions_kill_test.go's cross-process spawn pattern
// (spawnKillTestLockHolder) since only a genuine second process can prove
// the OS-level contention this function is designed to detect.
func TestLockHolderProvablyDead_StaleMtimeButLiveHolder_NotDeleted(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".crush")

	holder := spawnKillTestLockHolder(t, dataDir, "live-holder-stale-mtime")
	defer holder.stop()

	require.True(t, session.IsProcessAlive(holder.pid))

	// Backdate the lock file's mtime well past the 60s auto-delete
	// threshold, simulating exactly the scenario task #222 introduced: a
	// live holder whose heartbeat hasn't touched the file recently because
	// it's blocked in a long tool call with no recorded activity.
	lockPath := filepath.Join(dataDir, "locks", "session-live-holder-stale-mtime.lock")
	require.FileExists(t, lockPath)
	oldTime := time.Now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(lockPath, oldTime, oldTime))

	dead := lockHolderProvablyDead(dataDir, "live-holder-stale-mtime")
	assert.False(t, dead, "a genuinely live holder must not be reported as provably dead, even with a stale mtime")

	// The lock file must still be exactly as it was — this function must
	// never itself delete anything; it only reports true/false.
	require.FileExists(t, lockPath)
	assert.True(t, session.IsProcessAlive(holder.pid), "probe must never disturb a live holder")
}

// TestLockHolderProvablyDead_NoRealHolder_ReportsDead is the companion
// happy-path test: when nobody actually holds the OS lock (a genuinely
// abandoned lock file — process crashed or exited without Release), the
// probe must succeed and report the holder as provably dead so the
// auto-delete path can proceed.
func TestLockHolderProvablyDead_NoRealHolder_ReportsDead(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".crush")

	// Simulate an abandoned lock file: content written directly (no real OS
	// lock held by anyone), naming a plausible-looking but uncontended PID.
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-abandoned-id.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))

	dead := lockHolderProvablyDead(dataDir, "abandoned-id")
	assert.True(t, dead, "a lock file with no real OS-level holder must be reported as provably dead")
}

// TestSessionsLocksCmdRun_HonorsConfiguredDataDir is the regression test for
// task #231 finding 1: sessionsLocksCmdRun computed locksDir (and the
// lockHolderProvablyDead probe's dataDir) as filepath.Join(cwd, ".crush",
// ...), completely ignoring --data-dir / a configured data_directory, even
// though setupApp(cmd) had already resolved the correct value onto `a`.
// This is the same bug class task #219/#224 already fixed for `sessions
// kill` / `sessions reset --force` (see
// TestSessionsKillCmdRun_HonorsConfiguredDataDir in sessions_kill_test.go,
// whose isolation pattern this test mirrors), left unfixed for `sessions
// locks`.
//
// This points --data-dir at a directory deliberately outside cwd, seeds a
// real lock file at the path the FIX computes
// (<configured-data-dir>/locks/session-<id>.lock), and runs the real
// sessionsLocksCmd.RunE. Before the fix this prints "(no locks)" (having
// looked in the wrong, cwd-based directory); after the fix it must list the
// seeded session.
func TestSessionsLocksCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	// sessionsLocksCmdRun resolves the data directory via setupApp ->
	// config.Load/config.Init, which read real config paths from the
	// environment unless isolated — see isolateConfigEnvForTests's doc
	// comment (task #224 finding 3).
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".crush")
	// can never accidentally coincide with this path.
	configuredDataDir := filepath.Join(tmp, "elsewhere-data")

	ensureRootFlagStandIns(sessionsLocksCmd, configuredDataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	sessionsLocksCmd.SetContext(context.Background())

	const sessionID = "configured-datadir-locks-id"
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sessionID+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	// Fresh mtime so the auto-delete-if-stale path (age > 60s) never
	// triggers and this test's assertion is about listing, not survival.
	now := time.Now()
	require.NoError(t, os.Chtimes(lockPath, now, now))

	// Sanity: the WRONG (pre-fix) path must not exist, so "(no locks)"
	// being printed can never be confused with a real find.
	wrongPath := filepath.Join(workDir, ".crush", "locks", "session-"+sessionID+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	stdout := captureStdout(t, func() {
		runErr := sessionsLocksCmd.RunE(sessionsLocksCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions locks stdout:\n%s", stdout)

	require.NotContains(t, stdout, "(no locks)",
		"fix must find the lock at the --data-dir-configured location, not report none found")
	require.Contains(t, stdout, sessionID,
		"listing must include the session seeded at the configured data dir")
}

// TestSessionsLocksCmdRun_RemoveFailureAfterProvablyDead_Surfaced is the
// regression test for task #234: sessionsLocksCmdRun's auto-delete branch
// used to silently swallow an os.Remove failure after
// lockHolderProvablyDead(dataDir, sessionID) had already proven the holder
// dead — `if err := os.Remove(lockPath); err == nil { ...report... };
// continue` — printing nothing on the error path and unconditionally
// `continue`-ing, so the entry vanished from the listing entirely with zero
// operator-visible signal anything went wrong.
//
// This matters because lockHolderProvablyDead's own probe-then-release
// cycle (TryAcquireSessionLock/Release) truncates the lock file's content
// and freshens its mtime as a side effect of proving death (see that
// function's doc comment). If the immediately-following os.Remove then
// fails — a real, if narrow, possibility on Windows, whose lock files are
// opened without FILE_SHARE_DELETE — the file is left behind with a wiped
// PID and a FRESH mtime, which every PID-fallback/mtime-based liveness
// consumer in this codebase would read as LIVE, even though the holder was
// just proven dead moments earlier.
//
// This is a REAL end-to-end repro, not a synthetic error: a plain
// os.OpenFile handle held open on the lock file from this same test process
// is sufficient to make a subsequent os.Remove fail with a genuine sharing
// violation on Windows (confirmed directly: opening a file, taking no OS
// advisory lock at all, and calling os.Remove on it while the handle stays
// open fails the same way). Crucially this share-mode delete-block is
// independent of the LockFileEx advisory lock lockHolderProvablyDead
// acquires and releases — so the probe itself still succeeds and reports
// "provably dead" (the held-open handle doesn't contend for the advisory
// lock), while the caller's subsequent os.Remove on the same path still
// fails because of the open handle. That is exactly the fix's target
// scenario: Remove fails AFTER lockHolderProvablyDead already returned true.
func TestSessionsLocksCmdRun_RemoveFailureAfterProvablyDead_Surfaced(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("relies on Windows delete-sharing semantics (no FILE_SHARE_DELETE) to force a real os.Remove failure deterministically")
	}

	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "remove-fail-data")
	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	sessionsLocksCmd.SetContext(context.Background())

	const sessionID = "remove-fail-provably-dead-id"
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sessionID+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))

	// Backdate well past the 60s auto-delete threshold so the auto-delete
	// branch is entered.
	oldTime := time.Now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(lockPath, oldTime, oldTime))

	// Sanity: nobody holds the real OS lock, so lockHolderProvablyDead must
	// report true — this test is about what happens to the SUBSEQUENT
	// os.Remove, not about the probe itself. NOTE: this precondition call
	// itself performs a full acquire+release cycle, which (like the real
	// probe inside sessionsLocksCmdRun) freshens the lock file's mtime as a
	// side effect (see lockHolderProvablyDead's doc comment) — so the mtime
	// must be re-backdated afterward, or the real run below would see a
	// fresh mtime and never enter the auto-delete branch at all.
	require.True(t, lockHolderProvablyDead(dataDir, sessionID),
		"precondition: probe must report the holder provably dead before this test's Remove-failure scenario is meaningful")
	require.NoError(t, os.Chtimes(lockPath, oldTime, oldTime))

	// Hold a plain, unlocked handle open on the lock file. This blocks
	// os.Remove via Windows delete-sharing (confirmed independent of the
	// LockFileEx advisory lock the probe above already took and released).
	blocker, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	require.NoError(t, err)
	defer blocker.Close()

	stdout, stderr := captureStdoutAndStderr(t, func() {
		runErr := sessionsLocksCmd.RunE(sessionsLocksCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions locks stdout:\n%s", stdout)
	t.Logf("sessions locks stderr:\n%s", stderr)

	require.Contains(t, stderr, "warning: could not remove provably-dead lock",
		"a Remove failure after lockHolderProvablyDead returned true must be surfaced, never silently swallowed")
	require.Contains(t, stderr, "session-"+sessionID+".lock")

	// The entry must still show up in the listing (fell through to the
	// normal display path) rather than silently vanishing, since the file
	// genuinely still exists on disk with misleadingly fresh-looking
	// metadata.
	require.Contains(t, stdout, sessionID,
		"a lock whose removal failed must still appear in the listing, not silently vanish")

	// The file must still be on disk (Remove genuinely failed).
	require.FileExists(t, lockPath)
}

// TestSessionsLocksCmdRun_ConcurrentDeleteBeforeRemove_ENOENTIsSuccess is
// the deterministic regression test for task #244: task #234's fix (see
// TestSessionsLocksCmdRun_RemoveFailureAfterProvablyDead_Surfaced) correctly
// stopped silently swallowing a genuine os.Remove failure after
// lockHolderProvablyDead proved the holder dead, but over-corrected by
// treating EVERY os.Remove error — including fs.ErrNotExist — as
// warning-worthy. fs.ErrNotExist means a concurrent process (`sessions reap`,
// `sessions kill`, `sessions reset --force`, or another parallel `sessions
// locks` racing the same auto-delete path) already removed the file between
// lockHolderProvablyDead's probe and this Remove call. The goal ("stale lock
// gone from disk") is already achieved, so ENOENT here is success, not
// failure.
//
// Unlike the original version of this test (commit ba797493), which relied on
// a probabilistic goroutine race and could pass via the err==nil branch
// (production code removing the file first) without ever exercising the
// fs.ErrNotExist branch the fix targets, this version uses a test-only seam
// (preAutoDeleteRemoveHook) to deterministically delete the lock file BETWEEN
// lockHolderProvablyDead returning true and the os.Remove call, guaranteeing
// os.Remove always observes fs.ErrNotExist. This makes the regression
// assertion deterministic: reverting fix #244 (dropping the
// errors.Is(err, fs.ErrNotExist) case) causes this test to fail 100% of the
// time, not just when a coin flip lands the right way.
func TestSessionsLocksCmdRun_ConcurrentDeleteBeforeRemove_ENOENTIsSuccess(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "concurrent-delete-data")
	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	sessionsLocksCmd.SetContext(context.Background())

	const sessionID = "concurrent-delete-enoent-id"
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sessionID+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))

	// Backdate well past the 60s auto-delete threshold so the auto-delete
	// branch is entered.
	oldTime := time.Now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(lockPath, oldTime, oldTime))

	// Sanity: nobody holds the real OS lock, so lockHolderProvablyDead must
	// report true. Like TestSessionsLocksCmdRun_RemoveFailureAfterProvablyDead_Surfaced,
	// this precondition call itself performs a full acquire+release cycle
	// (freshening mtime as a side effect), so the mtime must be re-backdated
	// afterward or the real run below would never enter the auto-delete
	// branch at all.
	require.True(t, lockHolderProvablyDead(dataDir, sessionID),
		"precondition: probe must report the holder provably dead before this test's race scenario is meaningful")
	// The precondition probe's own Release() already truncated the file to
	// empty (see clearHolderMetadata) — restore non-empty placeholder
	// content so the test is meaningful (file exists before hook deletes it).
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))
	require.NoError(t, os.Chtimes(lockPath, oldTime, oldTime))

	// Install the test seam: delete the file between the probe and
	// os.Remove, deterministically forcing os.Remove to observe
	// fs.ErrNotExist — the exact TOCTOU window fix #244 targets.
	hookFired := false
	origHook := preAutoDeleteRemoveHook
	preAutoDeleteRemoveHook = func(path string) {
		require.Equal(t, lockPath, path)
		require.NoError(t, os.Remove(path))
		hookFired = true
	}
	t.Cleanup(func() { preAutoDeleteRemoveHook = origHook })

	stdout, stderr := captureStdoutAndStderr(t, func() {
		runErr := sessionsLocksCmd.RunE(sessionsLocksCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions locks stdout:\n%s", stdout)
	t.Logf("sessions locks stderr:\n%s", stderr)

	// CRITICAL: the hook MUST have fired — this proves the auto-delete
	// branch was reached and the file was deleted before os.Remove. Without
	// this assertion, a bug that skips auto-delete entirely would pass
	// silently.
	require.True(t, hookFired,
		"preAutoDeleteRemoveHook must fire; the auto-delete branch was not reached")

	require.NotContains(t, stderr, "warning: could not remove provably-dead lock",
		"a concurrent-delete (fs.ErrNotExist) must never be surfaced as a warning — the removal goal was already achieved by someone else")
	require.Contains(t, stderr, "removed stale lock",
		"an ENOENT race must still report the normal success message, exactly like an uncontested removal")
	require.Contains(t, stderr, "session-"+sessionID+".lock")

	// No phantom row: the file is gone, so it must not appear in the listing.
	require.NotContains(t, stdout, sessionID,
		"a lock removed via the ENOENT path must not leave a phantom row in the listing")

	// The file must genuinely be gone.
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file must be gone after the race, regardless of which side actually unlinked it")
}

// TestSessionsLocksCmdRun_AutoDeleteRemovesStaleLock is the happy-path
// companion to TestSessionsLocksCmdRun_ConcurrentDeleteBeforeRemove_ENOENTIsSuccess:
// when the production code's own os.Remove succeeds (err == nil, no concurrent
// deletion), the stale lock must be reported as removed and must not appear in
// the listing. This covers the other branch of the same switch case that the
// ENOENT test does not exercise, ensuring both success paths are covered
// separately rather than conflated by a single probabilistic test.
func TestSessionsLocksCmdRun_AutoDeleteRemovesStaleLock(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "auto-delete-data")
	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	sessionsLocksCmd.SetContext(context.Background())

	const sessionID = "auto-delete-stale-id"
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sessionID+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))

	// Backdate well past the 60s auto-delete threshold so the auto-delete
	// branch is entered.
	oldTime := time.Now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(lockPath, oldTime, oldTime))

	// Sanity: nobody holds the real OS lock, so lockHolderProvablyDead must
	// report true. Like TestSessionsLocksCmdRun_RemoveFailureAfterProvablyDead_Surfaced,
	// this precondition call itself performs a full acquire+release cycle
	// (freshening mtime as a side effect), so the mtime must be re-backdated
	// afterward or the real run below would never enter the auto-delete
	// branch at all.
	require.True(t, lockHolderProvablyDead(dataDir, sessionID),
		"precondition: probe must report the holder provably dead before this test's race scenario is meaningful")
	// The precondition probe's own Release() already truncated the file to
	// empty (see clearHolderMetadata) — restore non-empty placeholder
	// content so the test is meaningful (file exists before auto-delete).
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))
	require.NoError(t, os.Chtimes(lockPath, oldTime, oldTime))

	stdout, stderr := captureStdoutAndStderr(t, func() {
		runErr := sessionsLocksCmd.RunE(sessionsLocksCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions locks stdout:\n%s", stdout)
	t.Logf("sessions locks stderr:\n%s", stderr)

	require.NotContains(t, stderr, "warning: could not remove provably-dead lock",
		"a successful os.Remove (err == nil) must never be surfaced as a warning")
	require.Contains(t, stderr, "removed stale lock",
		"a successful os.Remove must report the normal success message")
	require.Contains(t, stderr, "session-"+sessionID+".lock")

	// No phantom row: the file is gone, so it must not appear in the listing.
	require.NotContains(t, stdout, sessionID,
		"a lock removed via the err==nil path must not leave a phantom row in the listing")

	// The file must genuinely be gone.
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file must be gone after auto-delete")
}

// captureStdoutAndStderr runs f while capturing both os.Stdout and
// os.Stderr, mirroring captureStdout/captureStderr individually. Needed
// here because the fix under test writes to BOTH streams in one invocation
// (the warning to stderr, the listing to stdout) and both must be asserted
// on from the same run.
func captureStdoutAndStderr(t *testing.T, f func()) (stdout string, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr

	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = outW
	os.Stderr = errW
	defer func() {
		os.Stdout = oldOut
		os.Stderr = oldErr
	}()

	var outBuf, errBuf bytes.Buffer
	doneOut := make(chan struct{})
	doneErr := make(chan struct{})
	go func() {
		_, _ = io.Copy(&outBuf, outR)
		close(doneOut)
	}()
	go func() {
		_, _ = io.Copy(&errBuf, errR)
		close(doneErr)
	}()

	f()
	_ = outW.Close()
	_ = errW.Close()
	<-doneOut
	<-doneErr
	return outBuf.String(), errBuf.String()
}

// TestSessionsLocksCmdRun_ReadsPIDViaSidecarFallback is the regression test
// for task #231 finding 2: sessionsLocksCmdRun read the PID column via a raw
// os.ReadFile(lockPath) + fmt.Sscanf, instead of the shared
// session.ReadLockPID helper used everywhere else in this codebase (e.g.
// sessions_kill.go's --force block). session.ReadLockPID (internal/session/
// lock.go's readLockFile) prefers the companion PID sidecar file
// (pidSidecarPath / writePIDSidecar) over the lock file's own content
// specifically because, on Windows, a live holder's mandatory LockFileEx
// range-lock can make the lock file's own content unreadable to a concurrent
// reader — the sidecar is written unlocked so it's always readable.
//
// Exercising the genuinely-unreadable-on-Windows case cross-platform isn't
// practical (that requires a real second process holding an OS range-lock,
// which is what TestLockHolderProvablyDead_StaleMtimeButLiveHolder_NotDeleted
// already spawns for a different assertion). Instead this test proves the
// mechanism the fix actually wires in: the sidecar is authoritative even
// when it DISAGREES with the raw lock file content (which the old
// Sscanf-based code could only ever have read). If sessionsLocksCmdRun ever
// regresses back to raw-parsing the lock file, this test catches it by
// asserting the sidecar's PID — not the lock file's stale/garbage content —
// is what ends up in the PID column.
func TestSessionsLocksCmdRun_ReadsPIDViaSidecarFallback(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "sidecar-data")
	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	sessionsLocksCmd.SetContext(context.Background())

	const sessionID = "sidecar-fallback-id"
	const sidecarPID = 424242
	const staleLockContentPID = 111111 // deliberately different from sidecarPID

	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sessionID+".lock")
	// The lock file's own content: a garbage/stale PID a raw Sscanf-based
	// reader would have returned.
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", staleLockContentPID)), 0o644))
	// The sidecar: what session.ReadLockPID actually prefers. Written via
	// the same mechanism TryAcquireSessionLock uses in production
	// (pidSidecarPath(lockPath) = lockPath + ".pid").
	require.NoError(t, os.WriteFile(lockPath+".pid", []byte(fmt.Sprintf("%d\n", sidecarPID)), 0o644))

	now := time.Now()
	require.NoError(t, os.Chtimes(lockPath, now, now))

	// Confirm session.ReadLockPID itself resolves to the sidecar value —
	// this is the exact helper the fix wires sessionsLocksCmdRun to call.
	require.Equal(t, sidecarPID, session.ReadLockPID(lockPath),
		"session.ReadLockPID must prefer the sidecar over the lock file's own content")

	stdout := captureStdout(t, func() {
		runErr := sessionsLocksCmd.RunE(sessionsLocksCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions locks stdout:\n%s", stdout)

	require.Contains(t, stdout, fmt.Sprintf("%d", sidecarPID),
		"PID column must reflect the sidecar-resolved PID (session.ReadLockPID), not the raw lock-file content")
	require.NotContains(t, stdout, fmt.Sprintf("\t%d\t", staleLockContentPID),
		"PID column must not fall back to a raw Sscanf of the lock file's own content when a sidecar disagrees")
}
