package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
