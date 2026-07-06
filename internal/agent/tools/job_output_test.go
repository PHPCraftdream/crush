package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestJobOutputTool_BoundedWaitReturnsWhileRunning proves that a `wait:true`
// call never blocks the agent turn indefinitely: when the underlying job is
// still running, the tool returns promptly (bounded by jobOutputMaxWait) with
// Status: running and a re-poll hint. We shrink jobOutputMaxWait for the test
// rather than sleeping for the real 90s window.
func TestJobOutputTool_BoundedWaitReturnsWhileRunning(t *testing.T) {
	// NOT t.Parallel(): this test mutates the package-global jobOutputMaxWait
	// (read by the tool at job_output.go), which the sibling
	// ...ReturnsCompletedWhenJobFinishes test also mutates. Running them in
	// parallel is a data race under -race (caught by CI's race-enabled build).
	workingDir := t.TempDir()
	ctx := context.Background()

	bgManager := shell.GetBackgroundShellManager()
	bgShell, err := bgManager.Start(ctx, workingDir, nil, "sleep 30", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = bgManager.Kill(context.Background(), bgShell.ID) })

	// Shrink the bound so the test returns in well under a second.
	originalMaxWait := jobOutputMaxWait
	jobOutputMaxWait = 100 * time.Millisecond
	t.Cleanup(func() { jobOutputMaxWait = originalMaxWait })

	tool := NewJobOutputTool()

	input, err := json.Marshal(JobOutputParams{ShellID: bgShell.ID, Wait: true})
	require.NoError(t, err)

	start := time.Now()
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  JobOutputToolName,
		Input: string(input),
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.False(t, resp.IsError)

	text := resp.Content
	require.Contains(t, text, "Status: running")
	// Phase 2: the header must always carry elapsed runtime.
	require.Contains(t, text, "elapsed")
	// Running jobs must NOT advertise an exit code.
	require.NotContains(t, text, "exit")
	require.Contains(t, text, "still running after the wait window")

	// Must return well under the real 90s bound — here we assert it didn't
	// even approach the 30s sleep, proving the wait was bounded.
	require.Less(t, elapsed, 5*time.Second, "bounded wait should return promptly, not block on the running job")
}

// TestJobOutputTool_BoundedWaitReturnsCompletedWhenJobFinishes proves that a
// job which completes inside the wait window is reported as completed, with
// an explicit exit code (0 on success) and elapsed runtime.
func TestJobOutputTool_BoundedWaitReturnsCompletedWhenJobFinishes(t *testing.T) {
	// NOT t.Parallel(): shares the package-global jobOutputMaxWait with the
	// sibling ...ReturnsWhileRunning test — see the note there.
	workingDir := t.TempDir()
	ctx := context.Background()

	bgManager := shell.GetBackgroundShellManager()
	bgShell, err := bgManager.Start(ctx, workingDir, nil, "echo 'all done'", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = bgManager.Kill(context.Background(), bgShell.ID) })

	// Give the quick command time to finish before we ask for output.
	require.Eventually(t, bgShell.IsDone, 5*time.Second, 25*time.Millisecond)

	originalMaxWait := jobOutputMaxWait
	jobOutputMaxWait = 100 * time.Millisecond
	t.Cleanup(func() { jobOutputMaxWait = originalMaxWait })

	tool := NewJobOutputTool()

	input, err := json.Marshal(JobOutputParams{ShellID: bgShell.ID, Wait: true})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  JobOutputToolName,
		Input: string(input),
	})

	require.NoError(t, err)
	require.False(t, resp.IsError)

	require.Contains(t, resp.Content, "Status: completed")
	// Phase 2: completed jobs always carry elapsed AND exit code (0 here).
	require.Contains(t, resp.Content, "elapsed")
	require.Contains(t, resp.Content, "exit 0")
	require.Contains(t, resp.Content, "all done")
	require.NotContains(t, resp.Content, "still running after the wait window")
}
