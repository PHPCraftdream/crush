package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunSubAgent_ChildSession_InheritsAutoApprove_DoesNotHangOnPermission is
// the end-to-end regression test for release-blocker P0-D (task #298, fixed
// by commit bd90f915).
//
// The bug: a non-interactive `crush run` auto-approves ONLY the root session
// id it was handed (app.go's Permissions.AutoApproveSession(sess.ID)). A
// sub-agent spawned via the "agent" tool runs under its OWN child session id
// (session.CreateAgentToolSessionID: "parentMessageID$$toolCallID") — a
// completely different map key from the parent's. Before the fix, nothing
// propagated the parent's auto-approve status to that child id, so the
// child's FIRST non-safe tool call (anything outside tools.safeCommands)
// reached permission.Request's final select, which blocks on either a UI
// response channel (nothing answers it in non-interactive mode) or
// ctx.Done() — hanging until the tool's own timeout (45 minutes in
// production) fired. This was the operator-reported symptom "the agent
// spawns a sub-agent and then nothing happens at all".
//
// The fix, runSubAgent's call to
// permissions.InheritSessionAutoApprove(params.SessionID, session.ID) right
// after the child session is created, propagates the parent's auto-approve
// status to the child id.
//
// This test proves the actual failure mode is closed — not merely that
// InheritSessionAutoApprove was called, which would just restate the
// production code rather than verify its effect (task #299's explicit
// requirement: prove the sub-agent is NOT blocked on a permission wait, not
// just that a function fired). It drives the REAL coordinator.runSubAgent
// (the single production entry point every ordinary "agent" tool delegation
// goes through) against a REAL permission.Service — the same type
// app.go wires up in production, not a mock — and a mock SessionAgent whose
// Run implementation performs a permission.Request for a NON-safe action
// (matching a real bash tool's shape: ToolName "bash", Action "execute",
// nothing in tools.safeCommands) against the CHILD session id runSubAgent
// created, using a ctx bounded to 2 seconds. If the request has to fall
// through to the blocking select (the pre-fix behavior), it will hang for
// the full 2 seconds and return granted=false (ctx.Done() branch) — the
// same terminal outcome production sees the tool watchdog eventually force,
// just observed on a much shorter, deterministic clock instead of waiting
// out a real 45-minute cap. If inheritance worked, Request returns
// immediately (granted=true) via the auto-approve branch, well within the
// bound.
//
// Verified by revert: commenting out the InheritSessionAutoApprove call in
// runSubAgent makes this test fail with "must not fall through to the
// blocking permission select" and a measured latency at (or past) the 2s
// bound — see the task report for the captured failure output.
func TestRunSubAgent_ChildSession_InheritsAutoApprove_DoesNotHangOnPermission(t *testing.T) {
	env := testEnv(t)

	const providerID = "p0d-probe"

	// A REAL permission.Service, exactly like production wires up — not a
	// stub. skip=false so Request takes its real code path instead of the
	// `if s.skip.Load() { return true, nil }` short-circuit that would make
	// this test pass vacuously regardless of the fix.
	permissions := permission.NewPermissionService(t.Context(), env.workingDir, false, nil, nil)

	parentSess, err := env.sessions.Create(t.Context(), "parent session P0-D")
	require.NoError(t, err)
	parentSessionID := parentSess.ID

	// Mirrors app.go's `app.Permissions.AutoApproveSession(sess.ID)` for the
	// PARENT/root session — exactly what `crush run` does on startup for the
	// one session id it was handed.
	permissions.AutoApproveSession(parentSessionID)

	// requestDone carries whether the permission.Request call returned
	// granted, and how long it took — the test's own probe, run from
	// INSIDE the mock agent's Run (i.e. from exactly where a real sub-agent's
	// bash tool call would perform its own permission.Request), so it
	// observes the child session id runSubAgent actually created.
	type probeResult struct {
		granted bool
		err     error
		elapsed time.Duration
	}
	probeDone := make(chan probeResult, 1)

	mock := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		// Bounded independently of the parent test context: 2s is far more
		// than enough for a granted-immediately auto-approve branch, and far
		// less than production's 45-minute tool watchdog, so a hang here is
		// still a fast, deterministic test failure instead of an actual
		// 45-minute wait.
		probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		start := time.Now()
		granted, reqErr := permissions.Request(probeCtx, permission.CreatePermissionRequest{
			SessionID:   call.SessionID, // the CHILD session id runSubAgent created
			ToolCallID:  "probe-tool-call",
			ToolName:    "bash",
			Description: "run a non-safe command",
			Action:      "execute", // NOT in tools.safeCommands — exactly the shape that hung in production
			Path:        env.workingDir,
		})
		probeDone <- probeResult{granted: granted, err: reqErr, elapsed: time.Since(start)}

		return agentResultWithText("sub-agent probe done"), nil
	})

	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.permissions = permissions

	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          mock,
		SessionID:      parentSessionID,
		AgentMessageID: "agent-msg-1",
		ToolCallID:     "tool-call-1",
		Prompt:         "do a task that requires a non-safe tool call",
		SessionTitle:   "P0-D sub-agent",
	})
	require.NoError(t, err)
	assert.False(t, resp.IsError, "runSubAgent must succeed end-to-end")

	var probe probeResult
	select {
	case probe = <-probeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the mock agent's Run never completed its permission probe — runSubAgent itself hung")
	}

	require.NoError(t, probe.err, "permission.Request must not return a context-deadline error")
	assert.True(t, probe.granted,
		"the child session's non-safe permission request must be auto-granted via inherited approval — "+
			"P0-D: before the fix, the child session id was absent from autoApproveSessions, so this request "+
			"fell through to the blocking select and could only resolve via ctx.Done() (false, deadline error)")
	assert.Less(t, probe.elapsed, 500*time.Millisecond,
		"the request must resolve immediately via the auto-approve branch, not by waiting out the 2s probe bound — "+
			"a near-2s elapsed time here means the request fell through to the blocking select and only returned "+
			"because THIS TEST'S bound fired, exactly the P0-D hang (just observed on a short clock instead of "+
			"production's real 45-minute tool watchdog)")
}

// TestRunSubAgent_ChildSession_NoInheritance_WhenParentNotAutoApproved is the
// non-regression companion: an INTERACTIVE parent (never auto-approved) must
// NOT have its sub-agent silently auto-approved either — inheritance must
// propagate "approved" but never manufacture approval that was never granted.
// Proves the fix is inheritance, not a blanket auto-approve regression.
func TestRunSubAgent_ChildSession_NoInheritance_WhenParentNotAutoApproved(t *testing.T) {
	env := testEnv(t)

	const providerID = "p0d-probe-2"

	permissions := permission.NewPermissionService(t.Context(), env.workingDir, false, nil, nil)
	// Deliberately NOT calling permissions.AutoApproveSession for the parent —
	// simulates an interactive session that was never auto-approved.

	parentSess, err := env.sessions.Create(t.Context(), "interactive parent session")
	require.NoError(t, err)
	parentSessionID := parentSess.ID

	type probeResult struct {
		granted bool
		elapsed time.Duration
	}
	probeDone := make(chan probeResult, 1)

	mock := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		granted, _ := permissions.Request(probeCtx, permission.CreatePermissionRequest{
			SessionID:  call.SessionID,
			ToolCallID: "probe-tool-call-2",
			ToolName:   "bash",
			Action:     "execute",
			Path:       env.workingDir,
		})
		probeDone <- probeResult{granted: granted, elapsed: time.Since(start)}
		return agentResultWithText("sub-agent probe done"), nil
	})

	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.permissions = permissions

	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          mock,
		SessionID:      parentSessionID,
		AgentMessageID: "agent-msg-2",
		ToolCallID:     "tool-call-2",
		Prompt:         "task",
		SessionTitle:   "non-inherited sub-agent",
	})
	require.NoError(t, err)

	probe := <-probeDone
	assert.False(t, probe.granted,
		"a child of a NON-auto-approved parent must not be silently granted — inheritance must propagate "+
			"approval, not manufacture it")
}
