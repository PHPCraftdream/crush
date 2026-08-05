package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestRunQueueTask_CorruptStdout_ReturnsErrorWithBoundedExcerpt is the
// regression test for task #272 (P1-3) finding 1:
// internal/cmd/queue.go's runQueueTask used to swallow a JSON-unmarshal
// failure on the spawned child's stdout, returning a nil error and zeroed
// metrics. The caller (queueRunCmd.RunE) took "err == nil" to mean the task
// succeeded and persisted StatusDone — a corrupted or empty child stdout
// looked identical to a healthy run.
//
// This calls runQueueTask directly (via the queueTaskExecOverride seam) with
// stdout that is not valid JSON, and with an oversized payload, asserting:
//  1. a non-nil error is returned (the pre-fix bug: err was always nil here),
//  2. the error text contains a BOUNDED excerpt of the bad stdout, not the
//     entire (potentially huge) output verbatim.
func TestRunQueueTask_CorruptStdout_ReturnsErrorWithBoundedExcerpt(t *testing.T) {
	task := queue.Task{ID: "task-corrupt-stdout", Prompt: "hi"}

	t.Run("garbled non-JSON output", func(t *testing.T) {
		badOutput := "not json at all, this is a crash banner or stderr bleed onto stdout"
		queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
			return []byte(badOutput), nil
		}
		t.Cleanup(func() { queueTaskExecOverride = nil })

		cost, tokens, exitReason, err := runQueueTask(context.Background(), t.TempDir(), t.TempDir(), task)

		require.Error(t, err, "corrupt/non-JSON stdout must be reported as an error, not silently treated as success")
		assert.Zero(t, cost)
		assert.Zero(t, tokens)
		assert.Empty(t, exitReason)
		assert.Contains(t, err.Error(), badOutput, "error must carry the (short) stdout excerpt for diagnostics")
	})

	t.Run("empty output", func(t *testing.T) {
		queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
			return []byte{}, nil
		}
		t.Cleanup(func() { queueTaskExecOverride = nil })

		_, _, _, err := runQueueTask(context.Background(), t.TempDir(), t.TempDir(), task)
		require.Error(t, err, "empty child stdout must be reported as an error, not silently treated as success")
	})

	t.Run("oversized bad output is truncated, not embedded verbatim", func(t *testing.T) {
		// 10x the excerpt bound used in runQueueTask (512 bytes), so a
		// pre-fix-style "embed the whole thing" implementation would produce
		// a much longer error message than the bounded version does.
		huge := make([]byte, 5*1024)
		for i := range huge {
			huge[i] = 'x'
		}
		queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
			return huge, nil
		}
		t.Cleanup(func() { queueTaskExecOverride = nil })

		_, _, _, err := runQueueTask(context.Background(), t.TempDir(), t.TempDir(), task)
		require.Error(t, err)
		assert.Less(t, len(err.Error()), len(huge),
			"error text must be a bounded excerpt of a huge stdout, not the entire payload")
	})
}

// TestQueueRunCmdRun_CorruptChildStdout_MarksTaskFailedNotDone is the
// end-to-end regression test for the same finding, exercised through the
// real queueRunCmd.RunE loop: a queued task whose spawned child returns
// non-JSON stdout must land in the queue DB as StatusFailed, never
// StatusDone, and the persisted exit reason/err must be visible to a later
// `queue list`/`queue show`.
func TestQueueRunCmdRun_CorruptChildStdout_MarksTaskFailedNotDone(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	origCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	dataDir := filepath.Clean(filepath.Join(tmp, "datadir"))

	// Seed one pending task via `queue add`.
	ensureRootFlagStandIns(queueAddCmd, dataDir)
	if f := queueAddCmd.Flags().Lookup("cwd"); f == nil {
		queueAddCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueAddCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueAddCmd.Flags().Set("cwd", "") })
	queueAddCmd.SetContext(context.Background())

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString("task with a misbehaving child")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	addErr := queueAddCmd.RunE(queueAddCmd, nil)
	os.Stdin = oldStdin
	require.NoError(t, addErr)

	// The spawned child "returns" garbage stdout instead of the expected
	// {"cost_usd":...} JSON envelope.
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
		return []byte("garbage, not json"), nil
	}
	t.Cleanup(func() { queueTaskExecOverride = nil })

	ensureRootFlagStandIns(queueRunCmd, dataDir)
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
		// stop-on-fail is false, so the command itself should not error even
		// though the one task inside it failed.
		require.NoError(t, runErr)
	})
	t.Logf("queue run stderr:\n%s", stderr)
	require.Contains(t, stderr, "processed 1 task(s)")

	// Read back the task status the same way an operator would: `queue list
	// --json`, reusing the real command against the same data dir.
	ensureRootFlagStandIns(queueListCmd, dataDir)
	if f := queueListCmd.Flags().Lookup("cwd"); f == nil {
		queueListCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueListCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueListCmd.Flags().Set("cwd", "") })
	if f := queueListCmd.Flags().Lookup("json"); f == nil {
		queueListCmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, queueListCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = queueListCmd.Flags().Set("json", "false") })
	if f := queueListCmd.Flags().Lookup("status"); f == nil {
		queueListCmd.Flags().String("status", "", "")
	}
	queueListCmd.SetContext(context.Background())

	stdout := captureStdout(t, func() {
		listErr := queueListCmd.RunE(queueListCmd, nil)
		require.NoError(t, listErr)
	})

	var got queue.Task
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "queue list --json output:\n%s", stdout)
	assert.Equal(t, queue.StatusFailed, got.Status,
		"a task whose child returned unparseable stdout must be recorded as failed, never done — pre-fix bug swallowed the parse error and reported StatusDone with zeroed metrics")
	assert.Zero(t, got.Cost)
	assert.Zero(t, got.Tokens)

	waitForSQLiteHandleRelease(t, dataDir)
}

// TestQueueRunCmdRun_UpdateStatusFailure_SurfacesError is the regression
// test for task #272 (P1-3) finding 2: queueRunCmd.RunE's batch-processing
// loop used to call `_ = q.UpdateStatus(...)` on both the success and
// failure branches, discarding any error UpdateStatus returned. A DB error
// writing the FINAL status (e.g. disk full, corruption, a dropped table)
// left the task's row stuck at status='running' forever — with no way for a
// later `queue list`/`queue show` to ever learn the task actually finished —
// while queueRunCmd.RunE itself returned nil (success) to its caller/exit
// code.
//
// To force a deterministic UpdateStatus failure without fighting SQLite
// concurrent-connection semantics, the queueTaskExecOverride callback (which
// runs synchronously inside runQueueTask, strictly BEFORE the parent loop's
// subsequent q.UpdateStatus call for the same task) opens its own short-lived
// connection to the exact same crush.db file and drops the queue_tasks
// table. By the time control returns to queueRunCmd.RunE's loop and it calls
// UpdateStatus, the table no longer exists, so the write fails every time —
// no timing race.
func TestQueueRunCmdRun_UpdateStatusFailure_SurfacesError(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	origCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	dataDir := filepath.Clean(filepath.Join(tmp, "datadir"))

	// Seed one pending task via `queue add`.
	ensureRootFlagStandIns(queueAddCmd, dataDir)
	if f := queueAddCmd.Flags().Lookup("cwd"); f == nil {
		queueAddCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueAddCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueAddCmd.Flags().Set("cwd", "") })
	queueAddCmd.SetContext(context.Background())

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString("task whose final status write will fail")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	addErr := queueAddCmd.RunE(queueAddCmd, nil)
	os.Stdin = oldStdin
	require.NoError(t, addErr)

	dbPath := filepath.Join(dataDir, "crush.db")

	// The child "succeeds" with valid JSON, but as a side effect drops the
	// queue_tasks table out from under the still-open app DB connection —
	// simulating a DB-level failure (corruption/disk issue/etc) that would
	// make the subsequent UpdateStatus call fail, without needing to
	// reproduce the underlying I/O failure itself.
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
		sabotage, sabErr := sql.Open("sqlite", dbPath)
		require.NoError(t, sabErr)
		defer sabotage.Close()
		_, sabErr = sabotage.Exec("DROP TABLE queue_tasks")
		require.NoError(t, sabErr, "sabotage connection must succeed in dropping queue_tasks for this test to be meaningful")

		out, _ := json.Marshal(map[string]any{
			"cost_usd":    0.01,
			"tokens":      int64(5),
			"exit_reason": "stop",
		})
		return out, nil
	}
	t.Cleanup(func() { queueTaskExecOverride = nil })

	ensureRootFlagStandIns(queueRunCmd, dataDir)
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
	// max-tasks=1 pins the outer "for maxTasks <= 0 || processed < maxTasks"
	// loop to a SINGLE ClaimPending + UpdateStatus round trip. Without this,
	// the sabotaged queue_tasks table (dropped by the exec override) would
	// also make the loop's NEXT ClaimPending call fail on its own SELECT —
	// that unrelated failure returns an error too, which would let this test
	// pass even if UpdateStatus's own error were still being silently
	// swallowed (confirmed by hand: reverting only the UpdateStatus handling
	// while leaving max-tasks=0 made this test a false positive, since the
	// error it observed was actually coming from the second ClaimPending,
	// not from UpdateStatus at all). Capping at one task means the ONLY
	// query issued after the sabotage is UpdateStatus itself.
	require.NoError(t, queueRunCmd.Flags().Set("max-tasks", "1"))
	queueRunCmd.SetContext(context.Background())

	runErr := queueRunCmd.RunE(queueRunCmd, nil)
	require.Error(t, runErr, "a DB error persisting the task's final status must surface as a queueRunCmd.RunE error, not be silently swallowed")
	assert.Contains(t, runErr.Error(), "final status",
		"the surfaced error should be traceable back to the UpdateStatus persistence step specifically, not some unrelated failure")
	assert.Contains(t, runErr.Error(), "queue_tasks",
		"the surfaced error should be traceable back to the actual DB failure")

	waitForSQLiteHandleRelease(t, dataDir)
}

// TestQueueRunCmdRun_DoesNotReclaimTaskFromLiveRunner is the mandatory
// regression test the task description calls out separately from the two
// above: a reclaim mechanism for orphaned 'running' tasks must NOT be able
// to steal a task from a runner that is alive but merely busy (task #269's
// exact lesson, applied to the queue instead of the session lock).
//
// This simulates a live `queue run` process by acquiring queue.lock and
// claiming one task into 'running' directly via the queue.Service — exactly
// what queueRunCmd.RunE itself does at the top of its loop — and holding
// that lock open on a background goroutine standing in for "still running,
// blocked on a long tool call". While that lock is held, it launches a
// SECOND queueRunCmd.RunE (representing a second `queue run` invocation,
// e.g. an operator or cron accidentally starting one while the first is
// still active) and asserts:
//
//  1. The second invocation does not complete, and the first runner's task
//     is NOT reclaimed to 'pending', for as long as the first lock is held —
//     acquireSpawnLock (queue.go) blocks on contention rather than failing
//     fast, so the second invocation cannot even reach the reclaim call
//     until the first lock is released.
//  2. Only after the simulated live runner finishes (releases queue.lock,
//     exactly like a real `queue run` process exiting/crashing) does the
//     second invocation proceed, reclaim the orphaned task, and complete.
//
// This is the direct behavioral proof that queue.ReclaimRunning's
// "authoritative because we already hold queue.lock" argument actually
// holds at runtime, not just in the doc comment.
func TestQueueRunCmdRun_DoesNotReclaimTaskFromLiveRunner(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	origCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	dataDir := filepath.Clean(filepath.Join(tmp, "datadir"))

	// Seed one pending task via `queue add`.
	ensureRootFlagStandIns(queueAddCmd, dataDir)
	if f := queueAddCmd.Flags().Lookup("cwd"); f == nil {
		queueAddCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueAddCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueAddCmd.Flags().Set("cwd", "") })
	queueAddCmd.SetContext(context.Background())

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString("task claimed by a live, merely-busy runner")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	addErr := queueAddCmd.RunE(queueAddCmd, nil)
	os.Stdin = oldStdin
	require.NoError(t, addErr)

	// --- Simulate a live `queue run` process ---
	//
	// Open the SAME sqlite file queueRunCmd.RunE itself will open (via its
	// own setupApp -> a.DB()), claim the task into 'running', and hold
	// queue.lock — exactly mirroring queueRunCmd.RunE's own sequence
	// (acquireSpawnLock, then ClaimPending) but staying "in the middle of a
	// tool call" indefinitely instead of finishing.
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath := filepath.Join(dataDir, "crush.db")
	liveConn, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	// Closed explicitly (not via t.Cleanup) right before
	// waitForSQLiteHandleRelease at the end of this test, so its own
	// RemoveAll-based release probe isn't racing an intentionally-still-open
	// second connection — t.Cleanup funcs run LIFO after the test body
	// returns, i.e. strictly after that call, which would make the probe
	// spin its full budget for nothing.

	_, err = liveConn.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS queue_tasks (
		id TEXT PRIMARY KEY,
		session_id TEXT,
		prompt TEXT NOT NULL,
		role TEXT,
		max_cost REAL,
		max_tokens INTEGER,
		timeout_sec INTEGER,
		status TEXT NOT NULL CHECK(status IN ('pending','running','done','failed','cancelled')),
		cost REAL DEFAULT 0,
		tokens INTEGER DEFAULT 0,
		exit_reason TEXT,
		created_at INTEGER NOT NULL,
		started_at INTEGER,
		finished_at INTEGER
	)`)
	require.NoError(t, err)

	liveQ := queue.NewService(liveConn)
	claimed, err := liveQ.ClaimPending(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1, "precondition: the live runner must have actually claimed the task before we test that it can't be stolen")
	liveTaskID := claimed[0].ID

	lockPath := filepath.Join(dataDir, "queue.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o755))
	release, err := acquireSpawnLock(lockPath)
	require.NoError(t, err, "precondition: the simulated live runner must hold queue.lock")

	// --- Launch a second `queue run` while the first is still "busy" ---
	ensureRootFlagStandIns(queueRunCmd, dataDir)
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

	// The second invocation's spawned-child path must never actually run
	// (there is nothing pending for it to claim while the live runner holds
	// the only task), but wire the override anyway so a test bug can't
	// accidentally shell out to a real `crush run`.
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
		t.Errorf("unexpected: second queue run spawned a child task while the live runner's task was still 'running' — it must have wrongly reclaimed task %s", liveTaskID)
		out, _ := json.Marshal(map[string]any{"cost_usd": 0.0, "tokens": int64(0), "exit_reason": "unexpected"})
		return out, nil
	}
	t.Cleanup(func() { queueTaskExecOverride = nil })

	secondRunDone := make(chan error, 1)
	go func() {
		secondRunDone <- queueRunCmd.RunE(queueRunCmd, nil)
	}()

	// Give the second invocation ample time to reach (and block on)
	// acquireSpawnLock. If it were able to proceed past the lock, it would
	// finish almost instantly (there's nothing else to wait on) — 500ms is
	// generous slack for a local sqlite file, nowhere near the minutes-long
	// legitimate busy windows this test is guarding against.
	select {
	case err := <-secondRunDone:
		t.Fatalf("second `queue run` completed (err=%v) while the first runner still held queue.lock — it must block until the lock is released, not race ahead and reclaim a live runner's task", err)
	case <-time.After(500 * time.Millisecond):
		// Expected: still blocked.
	}

	// While still blocked, confirm the task is still exactly where the live
	// runner left it: 'running', untouched.
	stillRunning, err := liveQ.Get(context.Background(), liveTaskID)
	require.NoError(t, err)
	assert.Equal(t, queue.StatusRunning, stillRunning.Status,
		"a task claimed by a live runner must not be reclaimed while that runner still holds queue.lock, no matter how long it's been 'running'")

	// Now let the simulated live runner "finish": release the lock (and
	// mark its task done, exactly like a normal successful queueRunCmd.RunE
	// would before releasing the lock via its own defer).
	require.NoError(t, liveQ.UpdateStatus(context.Background(), liveTaskID, queue.StatusDone, 0.01, 10, "stop"))
	release()

	select {
	case err := <-secondRunDone:
		require.NoError(t, err, "second `queue run` must complete cleanly once the lock is free")
	case <-time.After(60 * time.Second):
		// 60s, not 10s: sibling tests in this same file that exercise a real
		// setupApp/db.Connect/migrations round trip (e.g.
		// TestQueueRunCmdRun_ForwardsDataDirToSpawnedChild) were observed
		// taking 30+ seconds under `go test -race` on this machine — race
		// instrumentation adds substantial overhead to the exact
		// config.Init/db.Connect/migrations path the second queueRunCmd.RunE
		// goes through here. 10s produced a false failure under -race even
		// though the un-instrumented run completes in ~1s; 60s keeps slack
		// well above that observed worst case without materially weakening
		// the assertion (a second run that's actually stuck would still
		// time out the test).
		t.Fatal("second `queue run` never completed after the live runner released queue.lock")
	}

	require.NoError(t, liveConn.Close())
	waitForSQLiteHandleRelease(t, dataDir)
}
