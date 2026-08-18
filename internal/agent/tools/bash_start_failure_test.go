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

// TestBashTool_BackgroundShellStartLimitIsModelVisibleNotFatal pins the
// error-vs-response boundary at bash.go's two BackgroundShellManager.Start
// failure sites (the explicit run_in_background branch and the plain
// synchronous branch).
//
// Start's ONLY failure mode today is the MaxBackgroundJobs cap
// (internal/shell/background.go: the limit check is the sole error return;
// everything else in Start is infallible). That cap fails the invariance
// test for fatal errors: it is session state that changes on its own as
// jobs finish (activeJobs is decremented from each job's completion
// goroutine) and that the model can change on purpose via job_kill. The cap
// message even says "Please terminate or wait for some jobs to complete" —
// an instruction that can only be followed if it reaches the model.
//
// It used to be returned as a Go error instead, which fantasy escalates to
// isCriticalError (agent.go executeSingleTool) and executes as
// `return nil, err`, aborting the whole agent loop. Worst case shape: a
// session with 50 active background jobs (dev servers, watches) gets EVERY
// subsequent bash call killed — including plain foreground `ls` — with the
// model never learning why and nothing written to the session log.
//
// The assertion that matters is runErr == nil; message checks only confirm
// the model is told which of its options (wait vs job_kill) applies.
//
// NOT t.Parallel: it tops up and then drains the process-wide background
// shell manager singleton, which sequential tests share.
func TestBashTool_BackgroundShellStartLimitIsModelVisibleNotFatal(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	bgManager := shell.GetBackgroundShellManager()

	// Top the manager up to its cap regardless of what earlier tests left
	// running. sleep 60 holds each slot for the whole test; the guard bounds
	// the loop in case jobs unexpectedly complete mid-fill.
	fillDir := t.TempDir()
	for guard := 0; bgManager.ActiveJobs() < shell.MaxBackgroundJobs && guard < 2*shell.MaxBackgroundJobs; guard++ {
		_, err := bgManager.Start(context.Background(), fillDir, nil, "sleep 60", "job-limit fill")
		require.NoError(t, err, "filling up to the cap must succeed while below it")
	}
	require.Equal(t, shell.MaxBackgroundJobs, bgManager.ActiveJobs(),
		"manager must be at its cap before the tool is invoked, otherwise the test proves nothing")

	t.Cleanup(func() {
		killCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		bgManager.KillAll(killCtx)
	})

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "job-limit-session")

	run := func(t *testing.T, params BashParams) (fantasy.ToolResponse, error) {
		t.Helper()
		input, err := json.Marshal(params)
		require.NoError(t, err)
		return tool.Run(ctx, fantasy.ToolCall{
			ID: "job-limit-call", Name: BashToolName, Input: string(input),
		})
	}

	t.Run("explicit background start", func(t *testing.T) {
		resp, runErr := run(t, BashParams{
			Description:     "background start at the cap",
			Command:         "echo hi",
			RunInBackground: true,
		})
		require.NoError(t, runErr,
			"a returned error is what fantasy escalates to isCriticalError — it would "+
				"abort the whole agent loop over a condition the model can fix "+
				"(job_kill, waiting, or a foreground command)")
		require.True(t, resp.IsError, "the model still has to be told why nothing ran")
		require.Contains(t, resp.Content, "error starting background shell")
		require.Contains(t, resp.Content, "maximum number of background jobs",
			"the model needs the actual cap message to know its options")
	})

	t.Run("plain foreground command", func(t *testing.T) {
		resp, runErr := run(t, BashParams{
			Description: "foreground command at the cap",
			Command:     "echo hi",
		})
		require.NoError(t, runErr,
			"a returned error is what fantasy escalates to isCriticalError — a plain "+
				"foreground `echo` must not end the run just because background slots are full")
		require.True(t, resp.IsError, "the model still has to be told why nothing ran")
		require.Contains(t, resp.Content, "error starting shell")
		require.Contains(t, resp.Content, "maximum number of background jobs")
	})
}

// TestBashTool_CommandFailuresAreModelVisible pins the OTHER side of the
// classification: every error class a command itself can produce — parse
// failure, block-list rejection, non-zero exit, missing binary — already
// flows back to the model through formatOutput as an ordinary response,
// never as a fatal Go error. This is the live contract that makes bash.go's
// two `[Job %s] error executing command` returns dead code: their guard is
// `exitCode == 0 && execErr != nil`, and shell.ExitCode returns 0 only for
// a nil error (interp.ExitStatus is non-zero by construction and every
// other error maps to 1), so the guarded condition cannot be true.
//
// These are pinning tests, not regression tests for a change: they pass
// before and after the Start fix above. They exist so that a future edit to
// shell.ExitCode or the formatOutput path that silently reroutes command
// failures into fatal errors fails here, loudly.
func TestBashTool_CommandFailuresAreModelVisible(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "cmd-failure-session")

	cases := []struct {
		name    string
		command string
		want    string
	}{
		{"syntax error", `echo "unclosed`, "could not parse command"},
		{"blocked command", "sudo echo hi", "command is not allowed"},
		{"non-zero exit", "exit 7", "Exit code 7"},
		{"command not found", "crush-not-a-real-command-xyz", "Exit code 127"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(BashParams{
				Description: tc.name,
				Command:     tc.command,
			})
			require.NoError(t, err)

			resp, runErr := tool.Run(ctx, fantasy.ToolCall{
				ID: "cmd-failure-call", Name: BashToolName, Input: string(input),
			})
			require.NoError(t, runErr,
				"a command the model got wrong is model input, not infrastructure — "+
					"returning an error would kill the run instead of letting the model fix it")
			require.Contains(t, resp.Content, tc.want,
				"the failure detail must reach the model so it can react to it")
		})
	}
}

// TestBashTool_MissingSessionIDStaysFatal pins the one fatal error bash.go
// keeps on purpose: the session ID is injected into the tool-call context
// by the agent harness (internal/agent/agent.go), never by the model, so if
// it is missing it is missing for every subsequent call in the session too
// — invariant to retry, and exactly the wiring-bug class that must stay
// fatal rather than be retried into a loop. Mirrors
// TestTodosTool_InfrastructureErrorsStayFatal for the same rule.
func TestBashTool_MissingSessionIDStaysFatal(t *testing.T) {
	t.Parallel()

	tool := newBashToolForTest(t.TempDir())
	input, err := json.Marshal(BashParams{Description: "no session", Command: "echo hi"})
	require.NoError(t, err)

	_, runErr := tool.Run(context.Background(), fantasy.ToolCall{
		ID: "no-session-call", Name: BashToolName, Input: string(input),
	})
	require.Error(t, runErr,
		"a missing session ID is a wiring bug the model cannot retry its way out of")
	require.Contains(t, runErr.Error(), "session ID is required")
}
