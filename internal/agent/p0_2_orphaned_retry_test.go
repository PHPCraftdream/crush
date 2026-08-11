package agent

// P0-2 regression test for the orphaned-mailbox-work path
// (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// restartOrphanedWithRetry's own doc comment claimed calls are "durably
// enqueued... BEFORE returning control", but the original implementation
// spawned an unwaited goroutine per call and returned immediately — a
// process crash between that return and the goroutine's actual DB commit
// would lose the call permanently, with no error ever surfacing anywhere.
//
// The fix made restartOrphanedWithRetry synchronous (via sync.WaitGroup)
// relative to its own caller and gave it an error return, propagated by
// restartOrphaned via slog.Error. This test proves both: the function
// actually WAITS for the enqueue attempt (a fast-failing DB doesn't let it
// return before the attempt is resolved) and it reports failure via its
// return value instead of swallowing it silently.
//
// It deliberately does NOT go through the full mailbox/drainOrRelease path
// (like TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure does for
// startDetachedRun) — restartOrphanedWithRetry is called directly, since
// it is what actually changed and the mailbox plumbing above it is already
// covered by this session's many prior mailbox regression tests.

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestRestartOrphanedWithRetry_ReturnsErrorOnEnqueueFailure is the
// regression test for P0-2's orphaned-work path.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go's restartOrphanedWithRetry, change the signature back to
//     not return an error (drop `error` from the func signature and the
//     `return nil`/`return err` statements at the end), and have
//     restartOrphaned go back to calling it without checking a return value.
//  2. This test will fail to COMPILE (restartOrphanedWithRetry no longer
//     returns a value to assign to `err`) — a stronger signal than a
//     runtime failure, since Go's type system itself enforces the coupling
//     between this test and the fix.
//  3. Restore the error-returning signature and it compiles/passes again.
func TestRestartOrphanedWithRetry_ReturnsErrorOnEnqueueFailure(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "p0-2-orphaned-retry-test")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	call := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "orphaned work that must not vanish silently",
	}

	// Force every DB write (including EnqueueRunQueueEntry) to fail, the
	// same technique TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure
	// uses for startDetachedRun: release the pooled *sql.DB for this
	// dataDir so the next call against it errors instead of writing.
	require.NoError(t, db.Release(env.dbDir))

	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr,
		"restartOrphanedWithRetry must surface the enqueue failure instead of silently losing the call")
}

// TestRestartOrphanedWithRetry_WaitsForAllEnqueuesBeforeReturning proves the
// function is genuinely synchronous relative to its caller: with multiple
// orphaned calls, it must not return until every one of their enqueue
// attempts has been resolved (success or failure) — the original bug was
// exactly this: return before any goroutine's DB write had even started.
func TestRestartOrphanedWithRetry_WaitsForAllEnqueuesBeforeReturning(t *testing.T) {
	env := testEnv(t)

	sess1, err := env.sessions.Create(t.Context(), "p0-2-orphaned-retry-wait-1")
	require.NoError(t, err)
	sess2, err := env.sessions.Create(t.Context(), "p0-2-orphaned-retry-wait-2")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	calls := []SessionAgentCall{
		{SessionID: sess1.ID, Prompt: "orphaned call 1"},
		{SessionID: sess2.ID, Prompt: "orphaned call 2"},
	}

	require.NoError(t, sa.restartOrphanedWithRetry(calls),
		"with a live DB, both enqueues must succeed and the function must wait for both before returning")

	pending, err := env.sessions.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 2,
		"both orphaned calls must already be durably enqueued by the time restartOrphanedWithRetry returns — "+
			"proving it waited for the goroutines rather than returning before their DB writes committed")
}
