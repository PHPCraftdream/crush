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

// TestSessionsReapCmdRun_HonorsConfiguredDataDir is the regression test for
// task #233 finding 2: sessionsReapCmdRun used to compute locksDir as
// filepath.Join(ResolveCwd(cmd), ".crush", "locks"), completely ignoring
// --data-dir / a configured data_directory. Unlike sessions kill/list/watch,
// this command is purely filesystem-based (no DB), so the fix uses the
// lightweight config.ResolveDataDirectory helper (task #224) exactly like
// sessions_kill.go does, rather than pulling in setupApp.
//
// This points --data-dir at a directory deliberately outside --cwd, seeds a
// lock file there for an orphaned (PID guaranteed dead) session, ages it past
// the 10s heartbeat threshold, and runs the real sessionsReapCmd.RunE. Before
// the fix this would print "(no locks directory)" (having looked in the
// wrong, cwd-based directory); after the fix it must find and remove the
// orphan lock at the configured location.
func TestSessionsReapCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	// sessionsReapCmdRun now resolves the data directory via
	// config.ResolveDataDirectory, which — like config.Load/config.Init —
	// reads real config paths from the environment unless isolated. See
	// isolateConfigEnvForTests's doc comment (task #224 finding 3).
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".crush")
	// can never accidentally coincide with this path.
	configuredDataDir := filepath.Join(tmp, "elsewhere-data")

	ensureRootFlagStandIns(sessionsReapCmd, configuredDataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())

	const sessionID = "configured-datadir-reap-id"
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	// PID 999999 is guaranteed not to be a live process on any platform.
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))
	// Age it past the 10s heartbeat threshold so reap treats it as eligible
	// rather than "skip-young".
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	// Sanity: the WRONG (pre-fix) path must not exist, so a false pass via
	// "(no locks directory)" being silently treated as success is impossible
	// to confuse with the real assertion below.
	wrongPath := filepath.Join(workDir, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	stderr := captureStderr(t, func() {
		runErr := sessionsReapCmd.RunE(sessionsReapCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions reap stderr:\n%s", stderr)

	require.NotContains(t, stderr, "(no locks directory)",
		"fix must find the locks dir at the --data-dir-configured location, not report it missing")
	require.Contains(t, stderr, "reclaimed 1 lock",
		"fix must reap the orphan lock seeded at the configured data dir")

	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "orphan lock file at the configured data dir must be removed")
}
