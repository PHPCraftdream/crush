package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQueueRunCmdRun_HonorsConfiguredDataDir is the regression test for task
// #233 finding 5: queueRunCmd's RunE re-read the raw --data-dir flag value
// directly (falling back to filepath.Join(cwd, ".crush") only when the flag
// was EMPTY) instead of using the already-resolved a.Config().Options.
// DataDirectory that setupApp(cmd) had just computed — the same
// "prefer the raw flag over the resolved value" anti-pattern task #224 fixed
// in sessions_kill.go (finding 1).
//
// The cleanest repro is a configured data_directory in a project crush.json
// with NO --data-dir flag passed at all: the pre-fix code's empty-flag
// fallback branch computed filepath.Join(cwd, ".crush") unconditionally,
// completely ignoring the project's configured data_directory — even though
// setupApp's config.Init had already resolved the correct
// (config-driven) value onto `a`. (A relative --data-dir flag value alone
// doesn't reliably distinguish the two code paths here, because
// queueRunCmd's own setupApp(cmd) call — which runs before the buggy
// line — already performs the --cwd chdir as a side effect via
// config.Init -> ResolveCwd, so a bare relative flag string used directly
// happens to resolve against that same already-chdir'd process cwd. The
// configured-data_directory-in-crush.json case has no such coincidental
// overlap: the pre-fix fallback never looks at the config file at all.)
//
// This seeds a crush.json at the project dir with a non-default
// data_directory, runs the real queueRunCmd.RunE with --data-dir left empty,
// and asserts the queue lock file it creates (<dataDir>/queue.lock, left on
// disk by acquireSpawnLock/release) appears at the CONFIGURED location
// (<projectDir>/<configured-data-dir>/queue.lock) rather than the pre-fix
// <projectDir>/.crush/queue.lock guess.
func TestQueueRunCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	// Capture the real starting cwd BEFORE ResolveCwd (invoked inside RunE
	// via --cwd, both directly and through setupApp) os.Chdir()s the whole
	// process into projectDir — and restore it once the test is done. On
	// Windows a directory that is still the process's cwd cannot be
	// removed, which would otherwise make t.TempDir()'s cleanup fail to
	// delete projectDir.
	origCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	// A configured data_directory that is NOT ".crush" — the pre-fix
	// fallback's hardcoded guess — so the two are trivially distinguishable.
	const configuredDataDirName = "configured-via-crush-json"
	wantDataDir := filepath.Clean(filepath.Join(projectDir, configuredDataDirName))

	configJSON := `{"options": {"data_directory": "` + configuredDataDirName + `"}}`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "crush.json"), []byte(configJSON), 0o644))

	// Deliberately EMPTY --data-dir: the pre-fix bug only manifests on this
	// branch (its non-empty-flag branch used the flag value verbatim, which
	// is a different, already-covered case — see the doc comment above).
	ensureRootFlagStandIns(queueRunCmd, "")
	if f := queueRunCmd.Flags().Lookup("cwd"); f == nil {
		queueRunCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueRunCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueRunCmd.Flags().Set("cwd", "") })
	if f := queueRunCmd.Flags().Lookup("concurrent"); f == nil {
		queueRunCmd.Flags().Int("concurrent", 1, "")
	}
	require.NoError(t, queueRunCmd.Flags().Set("concurrent", "1"))
	require.NoError(t, queueRunCmd.Flags().Set("stop-on-fail", "false"))
	require.NoError(t, queueRunCmd.Flags().Set("max-tasks", "0"))
	queueRunCmd.SetContext(context.Background())

	stderr := captureStderr(t, func() {
		runErr := queueRunCmd.RunE(queueRunCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("queue run stderr:\n%s", stderr)
	require.Contains(t, stderr, "processed 0 task(s)")

	// Assert BEFORE any cleanup/handle-release helper touches wantDataDir —
	// waitForSQLiteHandleRelease works by actually calling os.RemoveAll on
	// its target as its "is it released yet" probe, so it must run only
	// once every assertion against the directory's contents is done.
	wantLockPath := filepath.Join(wantDataDir, "queue.lock")
	require.FileExists(t, wantLockPath,
		"queue lock must be created at the crush.json-configured data dir, not the pre-fix <cwd>/.crush guess")

	// Sanity: the WRONG (pre-fix) path — <projectDir>/.crush — must not have
	// a queue lock, so a false pass can't hide behind some coincidental
	// overlap.
	wrongLockPath := filepath.Join(projectDir, ".crush", "queue.lock")
	_, wrongStatErr := os.Stat(wrongLockPath)
	require.True(t, os.IsNotExist(wrongStatErr),
		"pre-fix wrong path (<cwd>/.crush) must not have a queue lock either, to keep the assertion meaningful")

	// queueRunCmd.RunE's own setupApp/a.Shutdown() already ran (deferred
	// inside RunE, which has returned by now), but on Windows the SQLite
	// file handle can take a moment to actually release — wait for it so
	// t.TempDir()'s cleanup (RemoveAll on tmp, which contains wantDataDir)
	// doesn't race a still-open handle. Must run LAST: it actually deletes
	// wantDataDir as its release-probe mechanism.
	waitForSQLiteHandleRelease(t, wantDataDir)
}
