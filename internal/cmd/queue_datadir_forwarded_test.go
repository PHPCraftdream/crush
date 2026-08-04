package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQueueRunCmdRun_ForwardsDataDirToSpawnedChild is the regression test for
// task #247: queueRunCmd's RunE resolves dataDir from the already-resolved
// a.Config().Options.DataDirectory (honoring an explicit --data-dir on the
// PARENT `queue run` invocation, or a configured data_directory — see
// queue_configured_datadir_test.go for that half), but runQueueTask — which
// spawns each task as its own `crush run --session ...` subprocess — never
// forwarded that resolved value on to the child. Each spawned child then
// re-resolved its OWN data dir from scratch starting at --cwd, which for an
// explicit parent --data-dir diverges from what the parent queue actually
// used: the child could read/write session state in a different DB than the
// one the queue claimed the task from.
//
// This seeds a parent `queue run` with an explicit --data-dir that differs
// from the cwd-based default (<cwd>/.crush), queues one task, runs the real
// queueRunCmd.RunE, and uses the queueTaskExecOverride test seam (queue.go)
// to intercept the argv that would have been passed to the spawned child
// crush run process — without actually spawning one — asserting
// "--data-dir <resolved-parent-dir>" is present verbatim.
func TestQueueRunCmdRun_ForwardsDataDirToSpawnedChild(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	origCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	// Explicit --data-dir on the PARENT, deliberately NOT <cwd>/.crush (the
	// default a child would independently re-derive if it ignored the
	// parent's resolved value entirely), so the two are trivially
	// distinguishable.
	explicitDataDir := filepath.Clean(filepath.Join(tmp, "explicit-parent-datadir"))

	ensureRootFlagStandIns(queueRunCmd, explicitDataDir)
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

	// Seed one pending task directly via `queue add`'s own command so the
	// task exists for `queue run` to claim, exercising the same app/db seam
	// queueRunCmd.RunE itself will reopen.
	ensureRootFlagStandIns(queueAddCmd, explicitDataDir)
	if f := queueAddCmd.Flags().Lookup("cwd"); f == nil {
		queueAddCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueAddCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueAddCmd.Flags().Set("cwd", "") })
	queueAddCmd.SetContext(context.Background())

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString("hello from task #247 regression test")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	addErr := queueAddCmd.RunE(queueAddCmd, nil)
	os.Stdin = oldStdin
	require.NoError(t, addErr)

	// Install the test-only exec seam (queue.go) to intercept the argv that
	// runQueueTask would otherwise hand to platform.Command/execCmd.Output,
	// capturing it instead of spawning a real `crush run` child process.
	var mu sync.Mutex
	var capturedArgs []string
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
		mu.Lock()
		capturedArgs = append([]string(nil), args...)
		mu.Unlock()
		out, _ := json.Marshal(map[string]any{
			"cost_usd":    0.0,
			"tokens":      int64(0),
			"exit_reason": "done",
		})
		return out, nil
	}
	t.Cleanup(func() { queueTaskExecOverride = nil })

	stderr := captureStderr(t, func() {
		runErr := queueRunCmd.RunE(queueRunCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("queue run stderr:\n%s", stderr)
	require.Contains(t, stderr, "processed 1 task(s)")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, capturedArgs, "queueTaskExecOverride must have been invoked for the queued task")

	found := false
	for i, a := range capturedArgs {
		if a == "--data-dir" && i+1 < len(capturedArgs) {
			require.Equal(t, explicitDataDir, capturedArgs[i+1],
				"spawned child's --data-dir must match the parent's own resolved data dir, not be omitted or re-derived")
			found = true
			break
		}
	}
	require.True(t, found, "spawned child argv must include --data-dir: %v", capturedArgs)

	waitForSQLiteHandleRelease(t, explicitDataDir)
}
