package agent

// P2-1 regression test (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// the durable-queue idempotency key must be stable across retries of the
// SAME logical call, not regenerated from time.Now().UnixNano() on every
// enqueue attempt — otherwise a caller-level retry creates a second durable
// row for what should be one logical request.
//
// SCOPE NOTE (added on independent review): the delegated /crush fix's own
// test file (this file, before this rewrite) asserted against a LOCAL
// closure that duplicated startDetachedRun/restartOrphanedWithRetry's key-
// generation logic rather than calling the real functions — passing
// regardless of what the actual production code did. This is the third
// occurrence of that exact pattern in this babygoal round (see commit
// 6bd8c927's and 3782c8fe's history for the first two). Rewritten below to
// call restartOrphanedWithRetry directly against a real DB.
//
// Also found and fixed while reviewing this: making the idempotency key
// stable exposes a real correctness gap the original delegated diff didn't
// address — internal/db/sql/run_queue.sql's EnqueueRunQueueEntry was a
// plain INSERT with no ON CONFLICT handling. A genuine retry reusing the
// same key would hit a PRIMARY KEY constraint violation (the row from the
// first attempt already exists) and be misclassified as a hard enqueue
// failure, triggering the in-memory data-loss-risk fallback for a call that
// had, in fact, already been durably enqueued successfully. Fixed by adding
// ON CONFLICT(id) DO NOTHING to the SQL query (regenerated via `sqlc
// generate`) and treating the resulting sql.ErrNoRows as success in
// session.EnqueueRunQueueEntry.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestSessionEnqueueRunQueueEntry_IdempotentOnSameKey is the direct,
// production-code-calling regression test for the ON CONFLICT DO NOTHING
// fix: enqueuing twice with the SAME idempotency key must succeed both
// times (not error on the second call) and must not create a second row.
//
// REVERT CHECK: remove `ON CONFLICT(id) DO NOTHING` from
// internal/db/sql/run_queue.sql's EnqueueRunQueueEntry query, run
// `sqlc generate`, and this test fails on the second EnqueueRunQueueEntry
// call with a UNIQUE constraint / primary key violation instead of nil.
// Restore the ON CONFLICT clause and regenerate to pass again.
func TestSessionEnqueueRunQueueEntry_IdempotentOnSameKey(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, "p2-1-idempotent-enqueue-test")
	require.NoError(t, err)

	key := uuid.New().String()
	callData := []byte(`{"Prompt":"first attempt"}`)

	require.NoError(t, env.sessions.EnqueueRunQueueEntry(ctx, key, sess.ID, callData),
		"first enqueue with a fresh key must succeed")
	require.NoError(t, env.sessions.EnqueueRunQueueEntry(ctx, key, sess.ID, callData),
		"retrying with the SAME key must succeed (idempotent), not error with a constraint violation")

	pending, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "two enqueues with the same key must produce exactly one durable row, not two")
}

// TestRestartOrphanedWithRetry_SameLogicalCallIDIsIdempotent is the
// end-to-end regression test: calling restartOrphanedWithRetry TWICE with a
// SessionAgentCall carrying the same LogicalCallID (simulating a caller-
// level retry of the same logical orphaned-work restart) must not create a
// duplicate durable row, and the second call must not report an error.
func TestRestartOrphanedWithRetry_SameLogicalCallIDIsIdempotent(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "p2-1-orphaned-retry-idempotent-test")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	call := SessionAgentCall{
		SessionID:     sess.ID,
		Prompt:        "orphaned work retried by the caller",
		LogicalCallID: uuid.New().String(),
	}

	require.NoError(t, sa.restartOrphanedWithRetry([]SessionAgentCall{call}),
		"first restart attempt must succeed")
	require.NoError(t, sa.restartOrphanedWithRetry([]SessionAgentCall{call}),
		"retrying the SAME logical call must still succeed (idempotent), not error")

	pending, err := env.sessions.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1,
		"two restart attempts for the same LogicalCallID must produce exactly one durable row")
}

// TestStartDetachedRun_LogicalCallIDGeneratedByBuildCall proves buildCall
// actually sets LogicalCallID on every call it constructs — the field this
// whole fix depends on being populated before startDetachedRun/
// restartOrphanedWithRetry ever see the call.
func TestStartDetachedRun_LogicalCallIDGeneratedByBuildCall(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)

	sess, err := env.sessions.Create(t.Context(), "p2-1-buildcall-logicalcallid-test")
	require.NoError(t, err)

	// buildCall's `pinned == nil` fallback reads c.currentAgent.Model(),
	// which this lightweight fixture doesn't set — resolve a real pinned
	// snapshot first, matching how every production caller invokes
	// buildCall post-UI-BUG-1 (pinned is never nil in practice anymore).
	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	call, err := coord.buildCall(t.Context(), sess.ID, "hello", pinned, nil)
	require.NoError(t, err)
	require.NotEmpty(t, call.LogicalCallID, "buildCall must populate LogicalCallID on every call it constructs")

	// Two separate buildCall invocations must NOT reuse the same ID —
	// LogicalCallID identifies one logical request, not the session.
	call2, err := coord.buildCall(t.Context(), sess.ID, "hello again", pinned, nil)
	require.NoError(t, err)
	require.NotEqual(t, call.LogicalCallID, call2.LogicalCallID,
		"each buildCall invocation is a distinct logical request and must get its own ID")
}
