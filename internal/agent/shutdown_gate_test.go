package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCancelAll_RefusesNewRunsAfterShutdown covers the closing review's
// blocker 1: the per-mailbox `stopped` latch stops an EXISTING turn loop
// from picking up more work, but it cannot stop a NEW owner from
// appearing.
//
// Two ways that happened before the agent-level gate:
//
//   - after CancelAll's sweep leaves a mailbox at mbIdle, submit() happily
//     grants ownership again (submit never consults `stopped`), so the next
//     Run performs a full provider turn that the already-finished sweep can
//     never cancel;
//   - mailboxes are created lazily and CancelAll iterates a COPY of the
//     map, so a Run for a session id the sweep never saw gets a brand new
//     mailbox with stopped == false. No race is even required for that one.
//
// Reachable callers after the sweep are real: coordinator's detached run
// (its own goroutine on a WithoutCancel context), runSummarize's tail,
// Summarize, and the web handlers — none of which look at mailbox state.
func TestCancelAll_RefusesNewRunsAfterShutdown(t *testing.T) {
	t.Run("mailbox latch alone does not stop a new owner", func(t *testing.T) {
		// Documents WHY the agent-level gate is needed rather than more
		// mailbox gating: after a hard stop and the era ending, the mailbox
		// is idle again and submit grants ownership to the next caller.
		mb := &mailbox{}
		mb.hardStop()
		mb.abandonOwnership(0)
		require.True(t, mb.isStopped(), "sanity: the latch is set")

		became, _ := mb.submit(SessionAgentCall{SessionID: "s1"}, func() {})
		require.True(t, became,
			"submit still grants ownership on a hard-stopped mailbox once its era ended — this is exactly why "+
				"the refusal has to live on the agent, before any mailbox is consulted")
	})

	t.Run("Run refuses on a session the sweep never saw", func(t *testing.T) {
		env := testEnv(t)
		sess, err := env.sessions.Create(t.Context(), "shutdown gate")
		require.NoError(t, err)

		agentIface := NewSessionAgent(SessionAgentOptions{
			Sessions: env.sessions,
			Messages: env.messages,
		})
		sa := agentIface.(*sessionAgent)

		// No mailbox exists for this session yet, so CancelAll's sweep has
		// nothing to latch — the pre-fix hole.
		sa.CancelAll()

		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "should never reach a provider",
			MaxOutputTokens: 100,
		})
		require.ErrorIs(t, runErr, ErrAgentShuttingDown,
			"after CancelAll, Run must refuse outright rather than claim the session and start a turn nothing "+
				"will cancel (closing review, blocker 1)")
	})

	t.Run("refusal is an explicit error, never a silent accept", func(t *testing.T) {
		env := testEnv(t)
		sess, err := env.sessions.Create(t.Context(), "shutdown gate explicit")
		require.NoError(t, err)

		agentIface := NewSessionAgent(SessionAgentOptions{
			Sessions: env.sessions,
			Messages: env.messages,
		})
		agentIface.CancelAll()

		res, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "p",
			MaxOutputTokens: 100,
		})
		// (nil, nil) is what "queued behind another owner" looks like. During
		// shutdown nothing will ever drain that queue, so returning it here
		// would be the same accepted-but-never-executed shape P0-A/P0-B
		// exist to remove.
		require.Nil(t, res)
		require.Error(t, runErr, "a silent (nil, nil) accept during shutdown would strand the call forever")
	})
}

// TestInterruptAndReplace_RefusesOnStoppedMailbox covers the closing
// review's blocker 2.
//
// interruptAndReplace only checked `state != mbOwned`. On a mailbox that
// had been hard-stopped but was still mbOwned it recorded the replacement
// and returned hadOwner=true — so the coordinator skipped its own
// no-owner path and the web client was told "queued". Both drains are
// then guaranteed to refuse it, and the payload is swept into an
// in-memory queue by a process that is exiting: accepted, never run,
// never reported.
func TestInterruptAndReplace_RefusesOnStoppedMailbox(t *testing.T) {
	mb := &mailbox{
		state:            mbOwned,
		epoch:            1,
		dispatcherCancel: func() {},
		current:          generation{id: 1, cancel: func() {}},
	}
	mb.hardStop()

	cancelFn, hadOwner := mb.interruptAndReplace(SessionAgentCall{SessionID: "s1", Prompt: "replace me"})

	require.False(t, hadOwner,
		"a hard-stopped mailbox must report no owner so the caller takes its own no-owner path (where Run "+
			"refuses visibly) instead of being told the interrupt was accepted (closing review, blocker 2)")
	require.Nil(t, cancelFn)
	require.Nil(t, mb.replacement,
		"nothing may be recorded on a stopped mailbox — both drains would refuse it, so it could only ever be "+
			"swept away unrun")
}
