package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_OSLockAcquisitionFailure_DoesNotPermanentlyWedgeMailbox is the
// end-to-end regression test for release-blocker BLOCKER-2 (round 9
// review), specifically the pre-loop bail-out case Run's cleanup defer
// exists for.
//
// Before the fix: tryReserveSession claims mailbox ownership, then OS-lock
// acquisition fails (a DIFFERENT process/holder already has the file lock),
// so Run returns via its cleanup defer WITHOUT ever entering the turn loop.
// The old defer called drainOrRelease/drainOrReleaseMerged unconditionally;
// on the (here, always-empty) "found something" branch that would have left
// the mailbox mbOwned forever, but even on the empty branch it relied on the
// mailbox not having moved on to a DIFFERENT owner in the meantime (no epoch
// check) — see the mailbox-level TestMailbox_AbandonOwnership_* tests for
// direct coverage of both branches in isolation. This test proves the fix
// holds at the Run() level: after an OS-lock failure, the session is NOT
// stuck permanently busy — a subsequent Run() call for the same session
// must succeed.
//
// NOTE (round 10 review, MEDIUM-1): nothing is ever queued in this
// scenario, so it does NOT exercise abandonOwnership's "found something"
// branch end-to-end — that branch is covered directly (and non-vacuously)
// by the mailbox-level TestMailbox_AbandonOwnership_WithQueuedWork_*
// tests. This test's own value is proving the empty-branch release+
// reclaim plumbing works through the real Run() call, which the isolated
// mailbox tests cannot do by themselves.
func TestRun_OSLockAcquisitionFailure_DoesNotPermanentlyWedgeMailbox(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "os-lock-failure test")
	require.NoError(t, err)

	// Hold the OS-level session lock externally, standing in for a
	// different process/holder — forces Run's own TryAcquireSessionLock to
	// fail, driving it into the pre-loop bail-out path.
	externalHolder, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err)

	srv := singleTurnSSEServer(nil)
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(t.Context(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(t.Context(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		LargeModel:           Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SmallModel:           Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	sa := agentIface.(*sessionAgent)

	// First call: OS lock is held externally, so this must fail at
	// acquisition and hit Run's pre-loop bail-out defer.
	_, err = agentIface.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "first attempt, should fail on the OS lock",
		MaxOutputTokens: 1000,
	})
	require.Error(t, err, "precondition: Run must fail while the OS lock is held externally")

	require.False(t, sa.IsSessionBusy(sess.ID),
		"the mailbox must NOT be left permanently owned after an OS-lock acquisition failure (BLOCKER-2a) — "+
			"before the fix, Run's cleanup defer could leave state==mbOwned with no turn loop ever going to run it again")

	// Release the external holder and retry — a SECOND Run() call for the
	// SAME session must succeed normally, proving the session was not
	// wedged busy by the first call's failure.
	require.NoError(t, externalHolder.Release())

	res, err := agentIface.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "second attempt, lock is free now",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	// Round 10 review, MEDIUM-1: (nil, nil) is ALSO err == nil, so asserting
	// only NoError passes vacuously in exactly the regressed case this test
	// claims to catch (a wedged session silently queues and Run returns
	// (nil, nil) without ever running). Assert a real result came back.
	require.NotNil(t, res, "a wedged session would return (nil, nil) here instead of an actual result — "+
		"NoError alone does not distinguish 'ran successfully' from 'silently queued behind a dead owner'")

	assert.False(t, sa.IsSessionBusy(sess.ID), "must be idle again after the second call completes")
}
