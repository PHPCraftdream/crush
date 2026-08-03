package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
