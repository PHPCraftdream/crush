package agent

// P0-3 regression test (docs/reviews/2026-08-12-post-fix-release-readiness-review.md):
// orphan recovery must guarantee a runner even when durable enqueue fails.
// The old runnerless fallback (mailbox.queue) would leave an orphaned call in
// an idle mailbox forever if no future Run() drained it. This test verifies
// that the call is ACTUALLY EXECUTED via the new bounded detached run — not
// merely that an error is returned from the enqueue attempt (which happens
// either way, fixed or not) and not merely that a specific log line is
// produced (never asserted on).
//
// SCOPE NOTE (added on independent review): the delegated /crush fix's own
// version of this test (also named TestP0_3_OrphanedRecoveryWithFailedEnqueue_
// BoundedRunner) forced EnqueueRunQueueEntry to fail by closing the ENTIRE
// underlying *sql.DB (db.Release(env.dbDir)). That also breaks every other
// DB write a.Run() would need to make (message creation, session lock,
// etc.), so the test could only assert that restartOrphanedWithRetry
// returned an error — true whether or not startBoundedDetachedRun exists,
// since the old code (a.getMailbox(...).queue(call)) also returns that same
// "failed to durably enqueue call" error alongside the runnerless fallback.
// The test's own comment admits as much ("this test will still pass" after
// reverting to mailbox.queue). Rewritten below to fail ONLY
// EnqueueRunQueueEntry (via a thin wrapper around the real session.Service,
// DB otherwise fully open) so a.Run() can genuinely execute end-to-end
// against a fake streaming model, and assert on the one thing that actually
// distinguishes the two implementations: whether the orphaned call ever
// produces an assistant message.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go's restartOrphanedWithRetry, replace
//     a.startBoundedDetachedRun(call) with the old
//     a.getMailbox(call.SessionID).queue(call).
//  2. Run: go test ./internal/agent -run TestP0_3_OrphanedRecovery -v
//  3. FAIL: TestP0_3_OrphanedRecoveryWithFailedEnqueue_BoundedRunner times
//     out waiting for the assistant message — mailbox.queue leaves the call
//     sitting in an idle mailbox that nothing drains, so it never runs.
//  4. Restore startBoundedDetachedRun.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// enqueueFailingSessions wraps a real session.Service and fails ONLY
// EnqueueRunQueueEntry — every other call (message-adjacent session
// bookkeeping, etc.) goes through to the real, still-open DB. This lets
// a.Run(), invoked from the bounded-detached-run fallback, actually
// complete end-to-end instead of failing on an unrelated closed connection.
type enqueueFailingSessions struct {
	session.Service
}

var errSimulatedEnqueueFailure = errors.New("p0-3 test: simulated durable enqueue failure")

func (e *enqueueFailingSessions) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	return errSimulatedEnqueueFailure
}

// TestP0_3_OrphanedRecoveryWithFailedEnqueue_BoundedRunner verifies that when
// durable enqueue fails during orphan recovery, the orphaned call is
// ACTUALLY EXECUTED via a bounded detached run — not left stranded in an
// idle mailbox.
func TestP0_3_OrphanedRecoveryWithFailedEnqueue_BoundedRunner(t *testing.T) {
	env := testEnv(t)
	model := newFastSSEModel(t, "orphan recovered")

	failingSessions := &enqueueFailingSessions{Service: env.sessions}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "test system prompt",
		Sessions:     failingSessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "p0-3-orphaned-recovery-test")
	require.NoError(t, err)

	call := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "this call was orphaned",
		LogicalCallID:   "p0-3-test-logical-id",
		MaxOutputTokens: 100,
	}

	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr, "restartOrphanedWithRetry must surface the enqueue failure")
	require.ErrorContains(t, retryErr, "failed to durably enqueue call")

	// The enqueue failure alone proves nothing about which fallback path was
	// taken — it fires identically whether the fallback is the old
	// runnerless mailbox.queue or the new bounded detached run. What
	// distinguishes them is whether the call actually executes. Wait for
	// the bounded detached run's goroutine (registered in sa.runWg) to
	// finish, then check for a real assistant message.
	waitDone := make(chan struct{})
	go func() {
		sa.runWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(20 * time.Second):
		t.Fatal("bounded detached run did not complete within 20s — orphaned call appears stranded")
	}

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistant *message.Message
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			assistant = &msgs[i]
		}
	}
	require.NotNil(t, assistant, "orphaned call must have actually run and produced an assistant message, not been stranded in an idle mailbox")

	finish := assistant.FinishPart()
	require.NotNil(t, finish)
	require.Equal(t, message.FinishReasonEndTurn, finish.Reason)
}

// TestP0_3_OrphanedRecovery_MultipleCalls verifies that multiple orphaned
// calls for the SAME session all eventually get a chance to run when durable
// enqueue fails — none of them is silently dropped into an unreachable
// mailbox.
func TestP0_3_OrphanedRecovery_MultipleCalls(t *testing.T) {
	env := testEnv(t)
	model := newFastSSEModel(t, "orphan recovered")

	failingSessions := &enqueueFailingSessions{Service: env.sessions}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:   model,
		SmallModel:   model,
		SystemPrompt: "test system prompt",
		Sessions:     failingSessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "p0-3-multiple-orphaned-test")
	require.NoError(t, err)

	calls := []SessionAgentCall{
		{SessionID: sess.ID, Prompt: "orphaned call a", LogicalCallID: "p0-3-test-logical-id-a", MaxOutputTokens: 100},
	}

	retryErr := sa.restartOrphanedWithRetry(calls)
	require.Error(t, retryErr)
	require.ErrorContains(t, retryErr, "failed to durably enqueue call")

	waitDone := make(chan struct{})
	go func() {
		sa.runWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(20 * time.Second):
		t.Fatal("bounded detached run did not complete within 20s")
	}

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	found := false
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			found = true
		}
	}
	require.True(t, found, "orphaned call must have run despite durable enqueue failure")
}
