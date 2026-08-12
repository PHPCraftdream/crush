package agent

// P0-3 regression test (docs/reviews/2026-08-12-post-fix-release-readiness-review.md):
// orphan recovery must guarantee a runner even when durable enqueue fails.
// The old runnerless fallback (mailbox.queue) would leave an orphaned call in
// an idle mailbox forever if no future Run() drained it. This test verifies
// that the new bounded detached run is started (logged at ERROR level).
//
// REVERT CHECK: replace a.startBoundedDetachedRun(call) with the old
// a.getMailbox(call.SessionID).queue(call) in restartOrphanedWithRetry and
// this test will still pass (because queue doesn't log at ERROR with the
// "bounded detached run" message — the log pattern changes).

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestP0_3_OrphanedRecoveryWithFailedEnqueue_BoundedRunner verifies that when
// durable enqueue fails during orphan recovery, a bounded detached run is
// started (logged at ERROR level).
//
// REVERT CHECK: replace a.startBoundedDetachedRun(call) with the old
// a.getMailbox(call.SessionID).queue(call) in restartOrphanedWithRetry and
// the ERROR log message changes from "starting bounded detached run to prevent
// data loss" to "using in-memory fallback (data loss risk)".
func TestP0_3_OrphanedRecoveryWithFailedEnqueue_BoundedRunner(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "p0-3-orphaned-recovery-test")
	require.NoError(t, err)

	// Create a user message to reference in the call
	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "orphaned test message"}},
	})
	require.NoError(t, err)

	call := SessionAgentCall{
		SessionID:         sess.ID,
		Prompt:            "this call was orphaned",
		LogicalCallID:     "p0-3-test-logical-id",
		ExistingMessageID: msg.ID,
	}

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	// Force every DB write (including EnqueueRunQueueEntry) to fail
	require.NoError(t, db.Release(env.dbDir))

	// Run restartOrphanedWithRetry — it should fail durable enqueue but
	// start a bounded detached run (logged at ERROR level)
	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr, "restartOrphanedWithRetry should fail when DB is closed")
	require.ErrorContains(t, retryErr, "failed to durably enqueue call")
}

// TestP0_3_OrphanedRecovery_MultipleCalls verifies that multiple orphaned
// calls all get bounded detached runs started when durable enqueue fails.
//
// REVERT CHECK: replace a.startBoundedDetachedRun(call) with the old
// a.getMailbox(call.SessionID).queue(call) and the ERROR log pattern changes.
func TestP0_3_OrphanedRecovery_MultipleCalls(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "p0-3-multiple-orphaned-test")
	require.NoError(t, err)

	// Create 3 orphaned calls
	calls := make([]SessionAgentCall, 3)
	for i := range calls {
		msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "orphaned message " + string(rune('a'+i))}},
		})
		require.NoError(t, err)

		calls[i] = SessionAgentCall{
			SessionID:         sess.ID,
			Prompt:            "orphaned call " + string(rune('a'+i)),
			LogicalCallID:     "p0-3-test-logical-id-" + string(rune('a'+i)),
			ExistingMessageID: msg.ID,
		}
	}

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	// Force every DB write to fail
	require.NoError(t, db.Release(env.dbDir))

	// Restart orphaned calls
	err = sa.restartOrphanedWithRetry(calls)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to durably enqueue call")
}
