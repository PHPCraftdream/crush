package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSessionsWhyCmdRun_HonorsConfiguredDataDir is the regression test for
// task #233 finding 4: sessionsWhyCmdRun passed the raw --cwd value into
// explainSessionStatus, which then computed the lock path as
// filepath.Join(cwd, ".crush", "locks", ...), ignoring --data-dir / a
// configured data_directory even though setupApp(cmd) had already resolved
// the correct value onto `a`. The fix passes a.Config().Options.DataDirectory
// instead (explainSessionStatus's dataDir parameter now expects the data
// directory itself, not a project cwd — see its updated doc comment and
// sessions_why_test.go's writeLockFile helper).
//
// This points --data-dir at a directory deliberately outside --cwd, seeds a
// live lock (this test process's own PID) ONLY at the configured location,
// and runs the real sessionsWhyCmd.RunE end-to-end. Before the fix this
// would report "status: at rest" (having looked for the lock at the wrong,
// cwd-based path and found nothing); after the fix it must report
// "status: running".
func TestSessionsWhyCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".crush")
	// (the pre-fix hardcoded guess) can never accidentally coincide with it.
	configuredDataDir := filepath.Join(tmp, "elsewhere-data")

	ensureRootFlagStandIns(sessionsWhyCmd, configuredDataDir)
	if f := sessionsWhyCmd.Flags().Lookup("cwd"); f == nil {
		sessionsWhyCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsWhyCmd.Flags().Set("cwd", ""))
	ctx := context.Background()
	sessionsWhyCmd.SetContext(ctx)

	built, err := setupApp(sessionsWhyCmd)
	require.NoError(t, err)
	require.Equal(t, configuredDataDir, built.Config().Options.DataDirectory,
		"test setup assumption: resolved DataDirectory must equal the --data-dir we configured")
	t.Cleanup(func() {
		dataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		waitForSQLiteHandleRelease(t, dataDir)
	})

	sess, err := built.Sessions.CreateWithID(ctx, "why-configured-datadir-id", "regression title")
	require.NoError(t, err)

	// Live lock (our own PID, guaranteed alive) ONLY at the configured data
	// dir.
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	now := time.Now()
	require.NoError(t, os.Chtimes(lockPath, now, now))

	// Sanity: the WRONG (pre-fix) path must not exist.
	wrongPath := filepath.Join(workDir, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	out := captureStdout(t, func() {
		runErr := sessionsWhyCmd.RunE(sessionsWhyCmd, []string{sess.ID})
		require.NoError(t, runErr)
	})
	t.Logf("sessions why stdout:\n%s", out)

	require.Contains(t, out, "status: running",
		"fix must find the lock at the --data-dir-configured location and report running, not at rest")
	require.NotContains(t, out, "status: at rest",
		"pre-fix behavior: looking up the wrong, cwd-based lock path would report at rest")
}
