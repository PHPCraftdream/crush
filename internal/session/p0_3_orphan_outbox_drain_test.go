package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestOrphanOutbox_Drainage tests that the pump drains orphan outbox entries
// to the main run queue and marks them as done.
func TestOrphanOutbox_Drainage(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-orphan-drain")

	// Write an entry directly to the orphan outbox (simulating a main queue write failure)
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "orphaned test prompt",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-outbox-test-1"
	err = svc.WriteToOrphanOutbox(t.Context(), outboxID, sess.ID, callDataJSON)
	require.NoError(t, err, "write to orphan outbox should succeed")

	// Verify the entry is pending in the outbox
	pendingOutbox, err := svc.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingOutbox, 1, "should have one pending outbox entry")
	require.Equal(t, outboxID, pendingOutbox[0].ID)

	// Create pump with fast drain tick for testing, but WITHOUT coordinator
	// so it drains but doesn't execute
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    nil, // No coordinator: drain but don't execute
		PumpInstanceID: "test-pump-drain",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 50 * time.Millisecond },
	})

	// Start the pump
	pump.Start()
	time.Sleep(200 * time.Millisecond) // Wait for at least a few drain ticks

	// Stop the pump
	pump.Stop()

	// Verify the outbox entry is gone (marked done and deleted)
	pendingOutbox, err = svc.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Empty(t, pendingOutbox, "outbox entry should be drained and marked done")

	// Verify the entry is in the main run queue
	pendingMain, err := svc.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingMain, 1, "entry should be in main run queue")
	require.Equal(t, sess.ID, pendingMain[0].SessionID)

	// Verify the call data matches
	var mainCallData map[string]any
	err = json.Unmarshal([]byte(pendingMain[0].CallData), &mainCallData)
	require.NoError(t, err)
	require.Equal(t, sess.ID, mainCallData["SessionID"])
	require.Equal(t, "orphaned test prompt", mainCallData["Prompt"])
}

// TestOrphanOutbox_ConcurrentDrainProtection tests that multiple pump instances
// can safely attempt to drain the same orphan outbox entry concurrently without
// data loss or duplication. In the new atomic model (P0-4 fix), there is no
// vulnerable 'processing' state — the drain operation is a single transaction.
//
// This test verifies the equivalent of the old TestOrphanOutbox_ClaimProtection:
// concurrent pumps cannot duplicate work or lose data. Only one pump succeeds
// (drained=true), others see it as already drained (drained=false).
func TestOrphanOutbox_ConcurrentDrainProtection(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-concurrent-drain")
	ctx := t.Context()

	// Write an entry to the orphan outbox
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "concurrent drain protection test",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-concurrent-test-1"
	err = svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, callDataJSON)
	require.NoError(t, err)

	// Simulate concurrent drain attempts from multiple pumps
	var wg sync.WaitGroup
	successCount := int32(0)
	failCount := int32(0)

	numPumps := 5
	for i := 0; i < numPumps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drained, err := svc.DrainOrphanOutboxEntry(ctx, outboxID)
			if err != nil {
				t.Errorf("drain failed: %v", err)
				return
			}
			if drained {
				atomic.AddInt32(&successCount, 1)
			} else {
				atomic.AddInt32(&failCount, 1)
			}
		}()
	}

	wg.Wait()

	// Verify exactly one pump succeeded
	require.Equal(t, int32(1), successCount, "exactly one pump should have successfully drained the entry")
	require.Equal(t, int32(numPumps-1), failCount, "all other pumps should have seen it as already drained")

	// Verify the entry is gone from the orphan outbox
	entry, err := svc.GetOrphanOutboxEntry(ctx, outboxID)
	require.NoError(t, err)
	require.Nil(t, entry, "entry should be gone from orphan outbox")

	// Verify the entry is in the main run queue exactly once
	pendingMain, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingMain, 1, "entry should be in main run queue exactly once")
	require.Equal(t, outboxID, pendingMain[0].ID)
	require.Equal(t, sess.ID, pendingMain[0].SessionID)
}

// TestOrphanOutbox_RevertCheck used to verify that orphan outbox entries
// remain pending forever with drainage "disabled" (a huge TestDrainTick).
// That invariant is no longer true BY DESIGN as of the P1-4 fix (docs/
// reviews/2026-08-13-release-readiness-static-audit.md §P1-4): every pump
// now attempts one drain immediately on Start(), regardless of
// TestDrainTick/OrphanOutboxDrainInterval -- a short-lived process must not
// depend on a periodic tick ever firing to recover pending outbox work.
// TestOrphanOutbox_InitialDrainOnStart directly covers that new invariant
// (entry present at Start() is drained immediately, not just voluntarily
// retested here). Rewriting this test to prove "an entry written strictly
// after Start() and before the first periodic tick stays pending" would
// require synchronizing with the Start()-time drain goroutine's completion
// -- there is no such hook, and sleep-based synchronization would just be a
// second flaky race on top of the one just found and fixed in P1-3's tests
// this same round. Deleted rather than kept as a disguised coin flip.

// transientDrainFailSessions wraps a real session.Service and fails the
// first N calls to DrainOrphanOutboxEntry, then passes through. Used to
// prove a transient (non-exhausted) drain failure doesn't strand the
// outbox entry.
//
// This wraps DrainOrphanOutboxEntry specifically — the actual call
// processOrphanOutboxEntry makes (run_queue_pump.go, P0-4 atomic drain,
// task #426) — not the old EnqueueRunQueueEntry/ClaimOrphanOutboxEntry
// pair the pre-P0-4 claim-based model used. A prior version of this mock
// wrapped EnqueueRunQueueEntry instead; that call is still on
// session.Service but DrainOrphanOutboxEntry's own transaction uses the
// DB-query-layer qtx.EnqueueRunQueueEntry internally, never the
// service-level method, so wrapping the service method silently stopped
// intercepting anything the moment #426 landed — found by independent
// review (docs/reviews/2026-08-13-release-readiness-static-audit.md
// review pass), not by this round's own verification.
type transientDrainFailSessions struct {
	session.Service
	failuresLeft int
}

func (s *transientDrainFailSessions) DrainOrphanOutboxEntry(ctx context.Context, id string) (bool, error) {
	if s.failuresLeft > 0 {
		s.failuresLeft--
		return false, errors.New("transientDrainFailSessions: simulated transient drain failure")
	}
	return s.Service.DrainOrphanOutboxEntry(ctx, id)
}

// TestOrphanOutbox_RetryAfterTransientFailure is a regression test: an
// outbox entry whose DrainOrphanOutboxEntry attempt fails transiently must
// still be reachable by a later drain cycle, not stranded.
//
// Under the P0-4 atomic-transaction drain (task #426), this now holds for
// a structural reason rather than needing dedicated retry/release logic:
// DrainOrphanOutboxEntry's transaction (BeginTx ... defer Rollback ...
// Commit) either fully applies or has no effect at all — a failure before
// Commit leaves the row exactly as it was, still 'pending', so the very
// next drain tick's ListPendingOrphanOutboxEntries scan picks it up again
// automatically. The pre-#426 claim-based model needed an explicit
// ReleaseOrphanOutboxEntryForRetry call for this same property, because a
// failed enqueue there left the row already moved to 'processing' by a
// separate, earlier step; that release call and the intermediate
// 'processing' state it existed to recover from are both gone now.
//
// REVERT CHECK: unlike most tests in this round, this one can't be
// reverted against a single line -- it verifies a structural property of
// BeginTx/Commit rather than a specific release call, and Go's
// database/sql transaction semantics make a genuinely "half-applied"
// state unobservable by construction. Manually verified instead: swapping
// pump.processOrphanOutboxEntry's DrainOrphanOutboxEntry call for the
// pre-#426 claim/enqueue/release sequence and re-running this test
// reproduces the exact symptom it guards against -- the entry stays in
// 'processing' and never reaches the main queue, since the injected
// failure now lands after the (now separate) claim step, an intermediate
// state that exists again once the atomic transaction is gone.
func TestOrphanOutbox_RetryAfterTransientFailure(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-orphan-retry")
	ctx := t.Context()

	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "retry after transient failure",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-retry-test-1"
	require.NoError(t, svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, callDataJSON))

	// Fail the first drain attempt, succeed on the second.
	flaky := &transientDrainFailSessions{Service: svc, failuresLeft: 1}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       flaky,
		Coordinator:    nil,
		PumpInstanceID: "test-pump-retry",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 50 * time.Millisecond },
	})

	pump.Start()
	time.Sleep(300 * time.Millisecond) // several drain ticks: first fails, later succeeds
	pump.Stop()

	// The entry must have been released back to pending after the first
	// failure and then successfully drained on a later cycle — not stuck
	// in 'processing'.
	pendingOutbox, err := svc.ListPendingOrphanOutboxEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pendingOutbox, "entry should have been drained after the transient failure was retried")

	entry, err := svc.GetOrphanOutboxEntry(ctx, outboxID)
	require.NoError(t, err)
	require.Nil(t, entry, "entry should be gone (marked done/deleted), not stuck in processing")

	pendingMain, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingMain, 1, "entry should have reached the main run queue after retry")
}

// TestOrphanOutbox_ConcurrentExecution tests that the drainage goroutine
// and the main tick goroutine can run concurrently without deadlocks or races.
func TestOrphanOutbox_ConcurrentExecution(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-concurrent-exec")
	ctx := t.Context()

	// Enqueue a normal entry to the main queue
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "normal queue test",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	err = svc.EnqueueRunQueueEntry(ctx, "normal-test-1", sess.ID, callDataJSON)
	require.NoError(t, err)

	// Write an entry to the orphan outbox
	orphanCallData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "orphan outbox test",
	}
	orphanCallDataJSON, err := json.Marshal(orphanCallData)
	require.NoError(t, err)

	outboxID := "orphan-concurrent-test-1"
	err = svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, orphanCallDataJSON)
	require.NoError(t, err)

	// Create pump and run it for a while. Uses a local call-counting
	// coordinator instead of the shared mockCoordinator: the completion
	// signal below needs a count of ACTUALLY EXECUTED calls, not queue
	// state (see the comment on the Eventually call for why).
	mockCoord := &countingCoordinator{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    mockCoord,
		PumpInstanceID: "test-pump-concurrent",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 30 * time.Millisecond },
	})

	pump.Start()

	// Wait for BOTH paths to actually finish, rather than assuming a fixed
	// sleep window is enough ticks for both. A fixed 500ms sleep here raced
	// on CI (windows-latest run 31735302674, consistently the
	// slowest/most-contended runner in this repo's matrix).
	//
	// The completion signal is the coordinator's OWN call counter reaching
	// 2 (one for normal-test-1, one for the drained orphan entry), NOT
	// "both pending lists are empty" -- an EARLIER version of this fix used
	// the pending-lists-empty check and it flaked locally (2/15 under
	// -race): a mid-flight lease (row status 'leased', not 'pending') reads
	// identically to "already gone" through ListPendingRunQueueEntries, so
	// the Eventually could return true the instant the drained orphan entry
	// was LEASED for its first execution attempt, before that attempt
	// actually completed. Stop() then raced the still-in-flight lease
	// ("lease failed ... err=context canceled" in the failing run's log)
	// and left the entry back in 'pending' with no more ticks left to
	// retry it. This exact ambiguity is already documented and avoided by
	// TestReleaseGate_P0_2_LockBusyNeverExhaustsRetries's call-counter
	// design (see its comment) -- reused here for the same reason.
	require.Eventually(t, func() bool {
		return mockCoord.calls.Load() >= 2
	}, 15*time.Second, 10*time.Millisecond,
		"both the normal queue entry and the drained orphan entry should eventually execute")

	forced := pump.Stop()

	// Verify graceful shutdown (no forced shutdown)
	require.False(t, forced, "pump should shut down gracefully")

	// Confirm both queues are empty once the pump has fully stopped --
	// wrapped in Eventually rather than a single check to allow for the
	// (now much smaller, near-instant) gap between "coordinator.Run()
	// returned" and "the ack commit lands", not because Stop() itself is
	// expected to race here.
	require.Eventually(t, func() bool {
		pendingMain, err := svc.ListPendingRunQueueEntries(ctx)
		if err != nil {
			return false
		}
		pendingOutbox, err := svc.ListPendingOrphanOutboxEntries(ctx)
		if err != nil {
			return false
		}
		return len(pendingMain) == 0 && len(pendingOutbox) == 0
	}, 2*time.Second, 10*time.Millisecond,
		"both queues should be empty once both entries have executed and been acked")
}

// countingCoordinator is a session.Coordinator that succeeds immediately
// (like mockCoordinator) while counting how many calls actually executed
// -- used where the test needs to know real execution happened, not just
// that a queue's pending-count transiently read zero (which a mid-flight
// lease also produces).
type countingCoordinator struct {
	calls atomic.Int64
}

func (c *countingCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, nil
}

// TestOrphanOutbox_CrashSafety verifies that the atomic drain operation
// provides crash safety: there is no vulnerable intermediate state where
// an entry can be left stranded after a process crash.
//
// TestOrphanOutbox_CrashSafety verifies that the atomic drain operation
// provides crash safety: there is no vulnerable intermediate state where
// an entry can be left stranded after a process crash.
//
// This test proves the atomic nature of DrainOrphanOutboxEntry:
// it attempts concurrent drains and verifies exactly one succeeds,
// demonstrating no entry can be left in a half-processed state.
func TestOrphanOutbox_CrashSafety(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-crash-safety")
	ctx := t.Context()

	// Write an entry to the orphan outbox
	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "crash safety test",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "crash-test-safety-1"
	err = svc.WriteToOrphanOutbox(ctx, outboxID, sess.ID, callDataJSON)
	require.NoError(t, err)

	// Simulate concurrent drain attempts (like multiple pump instances crashing)
	var wg sync.WaitGroup
	successCount := int32(0)
	failCount := int32(0)

	numConcurrentDrains := 5
	for i := 0; i < numConcurrentDrains; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drained, err := svc.DrainOrphanOutboxEntry(ctx, outboxID)
			if err != nil {
				t.Errorf("drain failed: %v", err)
				return
			}
			if drained {
				atomic.AddInt32(&successCount, 1)
			} else {
				atomic.AddInt32(&failCount, 1)
			}
		}()
	}

	wg.Wait()

	// Verify exactly one drain succeeded (atomic property)
	require.Equal(t, int32(1), successCount, "exactly one concurrent drain should succeed")
	require.Equal(t, int32(numConcurrentDrains-1), failCount, "all other drains should see it as already drained")

	// Verify the entry is gone from the orphan outbox
	entry, err := svc.GetOrphanOutboxEntry(ctx, outboxID)
	require.NoError(t, err)
	require.Nil(t, entry, "entry should be gone from orphan outbox")

	// Verify the entry is in the main run queue exactly once
	pendingMain, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingMain, 1, "entry should be in main run queue exactly once")
	require.Equal(t, outboxID, pendingMain[0].ID)

	// Verify the call data matches
	var mainCallData map[string]any
	err = json.Unmarshal([]byte(pendingMain[0].CallData), &mainCallData)
	require.NoError(t, err)
	require.Equal(t, sess.ID, mainCallData["SessionID"])
	require.Equal(t, "crash safety test", mainCallData["Prompt"])
}

// REVERT CHECK: comment out DrainOrphanOutboxEntry in session.go and this test will fail
// because the old claim-based model could leave entries in a stranded 'processing' state.

// TestOrphanOutbox_InitialDrainOnStart verifies that a pump attempts an
// orphan outbox drain immediately on Start(), not only on the first
// drainTicker.C fire (P1-4, docs/reviews/2026-08-13-release-readiness-
// static-audit.md §P1-4). Before the fix, a process living for less than
// TestDrainTick's interval (15s in production, OrphanOutboxDrainInterval)
// would start and exit without ever attempting to recover a pending outbox
// entry -- this simulates exactly that: a drain interval long enough that
// the test's own short lifetime would never observe a ticker fire.
//
// REVERT CHECK: remove the `if p.ctx.Err() == nil { p.drainOrphanOutbox() }`
// line before the drain goroutine's for-select loop in run_queue_pump.go,
// and this test fails (the entry stays pending the whole time, since the
// 10-minute TestDrainTick never fires within the test's lifetime).
func TestOrphanOutbox_InitialDrainOnStart(t *testing.T) {
	t.Parallel()

	sess, svc := setupTestSession(t, "test-initial-drain")

	callData := map[string]any{
		"SessionID": sess.ID,
		"Prompt":    "initial drain test",
	}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	outboxID := "orphan-initial-drain-test-1"
	err = svc.WriteToOrphanOutbox(t.Context(), outboxID, sess.ID, callDataJSON)
	require.NoError(t, err)

	// Drain interval deliberately far longer than this test's own lifetime,
	// so the only way the entry can be drained is via the initial,
	// Start()-time drain -- not via any drainTicker.C fire.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    nil,
		PumpInstanceID: "test-pump-initial-drain",
		TestTick:       func() time.Duration { return 10 * time.Millisecond },
		TestDrainTick:  func() time.Duration { return 10 * time.Minute },
	})

	pump.Start()
	require.Eventually(t, func() bool {
		pending, err := svc.ListPendingOrphanOutboxEntries(t.Context())
		return err == nil && len(pending) == 0
	}, 2*time.Second, 20*time.Millisecond, "entry should be drained by the initial Start()-time drain, not wait for drainTicker")
	pump.Stop()

	pendingMain, err := svc.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingMain, 1, "entry should have been moved to the main run queue")
	require.Equal(t, outboxID, pendingMain[0].ID)
}
