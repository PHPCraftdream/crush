// Regression coverage for task #204: `crush sessions reset --force` used to
// call forceKillHolder directly on whatever PID a lock file named, without
// first proving (via a real OS-level lock attempt) that the PID was still a
// genuine live holder. That is the exact stale/reused-PID bug already fixed
// for `crush sessions kill` in sessions_kill.go (see
// TestProbeThenKillHolder_StalePIDNotKilled) via the probeThenKillHolder
// helper — sessions.go's reset --force block was never updated to use it.
//
// This file proves the fix is actually wired into sessions reset --force's
// RunE, not just present as a standalone helper function: a lock whose
// holder already released (no live OS lock) but whose on-disk file still
// names our OWN test process's PID must survive `reset --force` — not be
// killed — exactly like the `sessions kill` regression test, but exercised
// through the real reset command end-to-end (setupApp -> resolveSessionID
// -> the --force block -> DeleteSessionMessages).
package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// isolateConfigEnvForTests sets XDG_DATA_HOME/CRUSH_GLOBAL_DATA,
// XDG_CONFIG_HOME/CRUSH_GLOBAL_CONFIG, and CRUSH_PROVIDER_CACHE_ONLY to
// throwaway directories under a fresh t.TempDir(), and points the default
// slog logger at io.Discard — the isolation this repo's CLAUDE.md documents
// as mandatory (not optional) for any test that reaches config.Load /
// config.Init / config.ResolveDataDirectory, since those read real config
// paths from the environment (GlobalConfig / GlobalConfigData) unless
// explicitly isolated. Without this, a test can silently read from or write
// to the operator's real global config/data — confirmed to have happened
// before.
//
// Returns the base tmp dir so callers can derive their own subdirectories
// (e.g. a configured --data-dir that lives outside it, or a workDir to
// os.Chdir into) without colliding with the env dirs this helper already
// created.
func isolateConfigEnvForTests(t *testing.T) (tmp string) {
	t.Helper()
	config.ResetProviderCacheForTests()

	tmp = t.TempDir()

	dataDir := filepath.Join(tmp, "global-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("CRUSH_GLOBAL_DATA", dataDir)

	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	crushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	return tmp
}

// isolatedResetEnv stands up a full app (same setupApp path `crush sessions
// reset` uses) in an isolated data dir/cwd, mirroring isolatedModelsEnv /
// runProvidersCmdInIsolatedApp's harness. Unlike those two, sessionsResetCmd's
// own RunE calls setupApp(cmd) a SECOND time (once here to seed the session,
// once inside RunE itself when the test invokes it), so the debug/data-dir/cwd
// stand-in flags and a real (non-nil) context must live directly on
// sessionsResetCmd itself, not a separate carrier command — see
// isolatedModelsEnv's doc comment for why a carrier doesn't work when RunE
// reads its own flags/context via the `cmd` it's called with. Returns the
// live *app.App (caller must NOT call a.Shutdown — this helper registers
// that via t.Cleanup so it runs before the temp-dir removal) and the
// workspace cwd (needed to compute the same lock path sessions.go's --force
// block writes/reads).
func isolatedResetEnv(t *testing.T) (a *app.App, cwd string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	ctx, cancel := context.WithCancel(context.Background())

	ensureRootFlagStandIns(sessionsResetCmd, "")
	require.NoError(t, sessionsResetCmd.Flags().Set("data-dir", ""))
	sessionsResetCmd.SetContext(ctx)

	built, err := setupApp(sessionsResetCmd)
	require.NoError(t, err)

	t.Cleanup(func() {
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(tmp)
		waitForSQLiteHandleRelease(t, tmp)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, workDir
}

// TestSessionsReset_ForceDoesNotKillStalePID is the load-bearing regression
// test for task #204: it creates a real session, writes a lock file at the
// exact path sessions.go's --force block computes
// (.crush/locks/session-<id>.lock) naming THIS TEST PROCESS's own PID, then
// runs the real sessionsResetCmd.RunE with --force. Before the fix, RunE
// called forceKillHolder(pid, ...) unconditionally on the recorded PID; since
// the OS lock for this session was never actually held (Release() ran
// cleanly / never acquired at all here), a fixed reset --force must probe
// first via probeThenKillHolder, see no live holder, and refuse to touch the
// PID — leaving the calling test process alive.
func TestSessionsReset_ForceDoesNotKillStalePID(t *testing.T) {
	a, cwd := isolatedResetEnv(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-stale-pid", "regression title")
	require.NoError(t, err)

	// Compute the exact lock path sessions.go's --force block reads/writes,
	// and pre-seed it with our own PID — simulating a holder that already
	// released (or an old-format lock file) without an actual live OS lock
	// behind it. Intentionally do NOT go through
	// session.TryAcquireSessionLock here: sessions.go's block only checks
	// os.Stat(lockPath) before proceeding, so a bare file with a stale PID
	// is exactly the shape it must handle safely.
	lockDir := filepath.Join(cwd, ".crush", "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	require.True(t, session.IsProcessAlive(os.Getpid()))

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "true"))
	stderr := captureStderr(t, func() {
		err := sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID})
		require.NoError(t, err)
	})
	t.Logf("reset --force stderr:\n%s", stderr)

	require.True(t, session.IsProcessAlive(os.Getpid()),
		"reset --force must never kill the calling test process via a stale lock-file PID")

	// Lock file must still be removed (the non-kill path still cleans up).
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file should be removed even when no live holder was found")
}

// resetSessionCmdFlags ensures the --force flag exists and is reset to its
// default before the test sets it, since sessionsResetCmd is a package-level
// var shared across tests/binary lifetime.
func resetSessionCmdFlags() *cobra.Command {
	if f := sessionsResetCmd.Flags().Lookup("force"); f != nil {
		_ = f.Value.Set(f.DefValue)
	}
	return sessionsResetCmd
}

// captureStderr (used above) is defined in claude_init_test.go — it captures
// os.Stderr output for the duration of a closure, which is exactly what
// sessionsResetCmd.RunE's --force block writes to via fmt.Fprint(os.Stderr,
// ...), mirroring sessions_kill.go's own reporting.

// isolatedResetEnvWithConfiguredDataDir mirrors isolatedResetEnv but points
// --data-dir at a directory that is deliberately NOT <workDir>/.crush — the
// exact gap task #219 exists to close. isolatedResetEnv always leaves
// --data-dir empty, so config.Load's default-to-<cwd>/.crush path silently
// makes "configured data dir" and "cwd/.crush" coincide, and the existing
// reset --force tests could never have caught a hardcoded cwd/.crush
// computation. This helper proves --data-dir is genuinely honored by
// sessions.go's --force block, not just present on the flag set.
func isolatedResetEnvWithConfiguredDataDir(t *testing.T) (a *app.App, workDir, dataDir string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir = t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	// The configured data dir lives entirely outside workDir, so
	// filepath.Join(workDir, ".crush") (the pre-fix hardcoded guess) can
	// never accidentally coincide with it.
	dataDir = filepath.Join(tmp, "configured-elsewhere-data")

	ctx, cancel := context.WithCancel(context.Background())

	ensureRootFlagStandIns(sessionsResetCmd, dataDir)
	sessionsResetCmd.SetContext(ctx)

	built, err := setupApp(sessionsResetCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory,
		"test setup assumption: resolved DataDirectory must equal the --data-dir we configured")

	t.Cleanup(func() {
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(tmp)
		waitForSQLiteHandleRelease(t, tmp)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, workDir, dataDir
}

// TestSessionsReset_ForceHonorsConfiguredDataDir is the regression test for
// task #219: sessions.go's --force block used to compute
// filepath.Join(cwd, ".crush") for both the lock path and the dataDir passed
// to probeThenKillHolder, ignoring --data-dir entirely. This test creates a
// session against a data dir that is NOT <cwd>/.crush, writes the lock file
// at the CONFIGURED location (mirroring the stale-PID pattern used by
// TestSessionsReset_ForceDoesNotKillStalePID), runs the real
// sessionsResetCmd.RunE with --force, and asserts the lock at the configured
// path was found and removed — proving the fix reads
// a.Config().Options.DataDirectory instead of recomputing cwd/.crush.
func TestSessionsReset_ForceHonorsConfiguredDataDir(t *testing.T) {
	a, _, dataDir := isolatedResetEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-configured-datadir", "regression title")
	require.NoError(t, err)

	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	require.True(t, session.IsProcessAlive(os.Getpid()))

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "true"))
	stderr := captureStderr(t, func() {
		err := sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID})
		require.NoError(t, err)
	})
	t.Logf("reset --force stderr:\n%s", stderr)

	require.NotContains(t, stderr, "warning: could not remove lock",
		"fix must find and remove the lock at the configured data dir, not miss it")
	require.Contains(t, stderr, dataDir,
		"report must reference the configured data dir's lock path, not a cwd-based guess")

	// Stale-PID safety must still hold at the configured location: our own
	// live test process must never be killed.
	require.True(t, session.IsProcessAlive(os.Getpid()),
		"reset --force must never kill the calling test process via a stale lock-file PID")

	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file at the configured data dir must be removed")
}

// TestSessionsReset_ForceStillKillsLiveHolder is the "didn't break the happy
// path" companion, mirroring TestProbeThenKillHolder_LiveHolderStillKilled:
// a real second process genuinely holds the session's OS lock, so reset
// --force must still detect contention and kill it, exactly as before this
// fix.
func TestSessionsReset_ForceStillKillsLiveHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	a, cwd := isolatedResetEnv(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-live-holder", "regression title")
	require.NoError(t, err)

	dataDir := filepath.Join(cwd, ".crush")
	holder := spawnKillTestLockHolder(t, dataDir, sess.ID)
	defer holder.stop()

	require.True(t, session.IsProcessAlive(holder.pid))

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "true"))
	stderr := captureStderr(t, func() {
		err := sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID})
		require.NoError(t, err)
	})
	t.Logf("reset --force stderr:\n%s", stderr)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && session.IsProcessAlive(holder.pid) {
		time.Sleep(50 * time.Millisecond)
	}
	require.False(t, session.IsProcessAlive(holder.pid), "a genuinely live holder must still be killed by reset --force")
}
