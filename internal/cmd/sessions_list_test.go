package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolatedListEnvWithConfiguredDataDir mirrors
// isolatedResetEnvWithConfiguredDataDir (sessions_reset_test.go) but wires up
// sessionsListCmd instead: stands up a real app via setupApp against a data
// directory that is deliberately NOT <cwd>/.crush, so a fix that reads
// a.Config().Options.DataDirectory can be told apart from a pre-fix
// cwd-based guess.
func isolatedListEnvWithConfiguredDataDir(t *testing.T) (a *app.App, workDir, dataDir string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir = t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".crush")
	// (the pre-fix hardcoded guess) can never accidentally coincide with it.
	dataDir = filepath.Join(tmp, "configured-elsewhere-data")

	ctx, cancel := context.WithCancel(context.Background())

	ensureRootFlagStandIns(sessionsListCmd, dataDir)
	if f := sessionsListCmd.Flags().Lookup("json"); f == nil {
		sessionsListCmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, sessionsListCmd.Flags().Set("json", "false"))
	sessionsListCmd.SetContext(ctx)

	built, err := setupApp(sessionsListCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory,
		"test setup assumption: resolved DataDirectory must equal the --data-dir we configured")

	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, workDir, dataDir
}

// TestSessionsListCmdRun_StatusHonorsConfiguredDataDir is the regression test
// for task #233 finding 1: computeSessionStatuses (backing `sessions list`'s
// STATUS column) used to compute locksDir as
// filepath.Join(ResolveCwd(cmd), ".crush", "locks"), completely ignoring
// --data-dir / a configured data_directory, even though sessionsListCmd's
// RunE had already resolved the correct value onto `a` via setupApp. This is
// the same bug class already fixed for `sessions kill` / `sessions reset
// --force` (task #219/#224) and `sessions locks` (task #231).
//
// This creates a real session, writes a lock file at the path the FIX
// computes (<configured-data-dir>/locks/session-<id>.lock) naming this test
// process's own live PID, runs the real sessionsListCmd.RunE, and asserts
// the STATUS column reports "running" for that session — not blank, which is
// what the pre-fix code would show since it was looking in the wrong,
// cwd-based directory that has no lock file at all.
func TestSessionsListCmdRun_StatusHonorsConfiguredDataDir(t *testing.T) {
	a, workDir, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-status-configured-datadir", "regression title")
	require.NoError(t, err)

	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	// Sanity: the WRONG (pre-fix) path must not exist, so a blank STATUS
	// column can never be confused with a real find.
	wrongPath := filepath.Join(workDir, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	stdout := captureStdout(t, func() {
		runErr := sessionsListCmd.RunE(sessionsListCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions list stdout:\n%s", stdout)

	require.Contains(t, stdout, sess.ID[:8],
		"listing must include the seeded session")

	// Find the row for our session and assert its STATUS column says
	// "running" (our own PID is alive) rather than being blank/dash, which
	// is what the pre-fix cwd-based lookup would produce since it never
	// finds the lock file seeded at the configured data dir.
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, sess.ID[:8]) {
			continue
		}
		found = true
		require.Contains(t, line, "running",
			"STATUS column must show running — fix must look up the lock at the configured data dir, not a cwd-based guess")
	}
	require.True(t, found, "expected to find the seeded session's row in the table output")
}

// TestComputeSessionStatuses_PidReuseBeyondMaxFallbackAgeIsNotRunning is the
// regression test for task #241: computeSessionStatuses trusted a
// CONFIRMED-alive recorded PID unconditionally, with no bound on how old the
// lock file itself could be. A `crush run` killed with SIGKILL leaves its
// PID in the lock file without releasing; hours later the OS can recycle
// that exact PID number for a completely unrelated, currently-running
// process. Before this fix, `sessions list` would report that session's
// STATUS as "running" forever — the crash never self-heals to "crashed"/
// "done" the way task #235 already fixed for session.InspectSessionLock.
//
// A real second process (spawnKillTestLockHolder, the same cross-process
// harness sessions_kill_test.go uses) stands in for "the OS reused this
// exact PID number" — it is genuinely alive throughout, so this proves the
// AGE bound, not merely a dead-PID false negative. The lock file's mtime is
// back-dated past session.MaxPidFallbackAge to simulate a lock abandoned
// long enough ago that its recorded PID can no longer be trusted.
func TestComputeSessionStatuses_PidReuseBeyondMaxFallbackAgeIsNotRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-status-pid-reuse", "regression title")
	require.NoError(t, err)

	holder := spawnKillTestLockHolder(t, dataDir, sess.ID)
	defer holder.stop()
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	staleTime := time.Now().Add(-(session.MaxPidFallbackAge + 5*time.Second))
	require.NoError(t, os.Chtimes(lockPath, staleTime, staleTime),
		"back-dating mtime past MaxPidFallbackAge to simulate a lock abandoned long enough ago that its recorded PID can no longer be trusted, even though it currently resolves to a live process")

	statuses := computeSessionStatuses(a)
	require.NotNil(t, statuses)
	assert.Equal(t, "crashed", statuses[sess.ID],
		"a lock older than MaxPidFallbackAge must not be reported running just because its recorded PID currently belongs to a live (but unrelated) process — this is the core #241 fix")
}

// TestComputeSessionStatuses_PidAliveWithinMaxFallbackAgeIsRunning is the
// non-regression companion: a live PID within MaxPidFallbackAge of the
// lock's mtime must still be reported "running", exactly like before this
// fix — the bound must only kick in once the lock is genuinely old.
func TestComputeSessionStatuses_PidAliveWithinMaxFallbackAgeIsRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-status-pid-fresh", "regression title")
	require.NoError(t, err)

	holder := spawnKillTestLockHolder(t, dataDir, sess.ID)
	defer holder.stop()
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	justUnder := time.Now().Add(-(session.MaxPidFallbackAge - 2*time.Minute))
	require.NoError(t, os.Chtimes(lockPath, justUnder, justUnder))

	statuses := computeSessionStatuses(a)
	require.NotNil(t, statuses)
	assert.Equal(t, "running", statuses[sess.ID],
		"a live PID just under MaxPidFallbackAge must still be trusted as running")
}
