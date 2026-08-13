// P0-3 regression test (docs/reviews/2026-08-13-release-readiness-static-audit.md):
// the lease watchdog must fire based on the ABSOLUTE lease_expires_at value
// actually committed to the DB, not on "wall-clock time since the last
// successful RenewRunQueueLease call RETURNED".
//
// The bug: RenewRunQueueLease's renewal loop computed
// `newExpiresAt := time.Now().Add(TTL)` BEFORE issuing the DB call, and that
// exact value is what gets written to the row's lease_expires_at on success.
// But the (now-fixed) watchdog used to track `lastSuccessfulRenewal =
// time.Now()` AFTER the call returned, and fire at
// `lastSuccessfulRenewal + TTL - safety_margin`. For a renewal call that
// takes duration D to complete successfully (DB contention, GC pause, disk
// stall), those two timestamps diverge by D. If D exceeds the safety margin,
// the watchdog fires AFTER the row's real DB expiry has already passed —
// during that gap, another pump instance's CleanupExpiredLeases can
// legitimately reclaim the row and dispatch a DUPLICATE execution while this
// executor is still running, believing itself safely within its lease.
//
// The fix: track the absolute lease_expires_at (seeded from the row's
// initial lease returned by LeaseRunQueueEntry, then updated on each
// successful renewal to the exact newExpiresAt value that was just written
// to the DB — never to a post-call time.Now()). The watchdog compares
// wall-clock now() directly against that absolute deadline minus the safety
// margin.
//
// NOTE on timing granularity: lease_expires_at is persisted as UNIX SECONDS
// (RenewRunQueueLease/LeaseRunQueueEntry both call .Unix()), so there is up
// to ~1s of truncation noise between the in-memory floating-point deadline
// and what lands in the DB. The parameters below (TTL=12s, margin=2s,
// artificial renewal delay=4s) are chosen with enough headroom that this
// truncation cannot flip either assertion — see the arithmetic in the doc
// on each constant.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go, change the renewal loop's success path back to
//     `leaseExpiresAtAtomic.Store(time.Now().UnixNano())` (post-call time)
//     instead of `time.Unix(newExpiresAt, 0).UnixNano()` (the pre-call value
//     actually written to the DB).
//  2. This test will FAIL: cancellation will be observed AFTER the row's
//     real DB lease_expires_at has already passed, not before it.

package session_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// delayedRenewalService wraps session.Service so that:
//   - renewal attempt #1 is delayed by a fixed duration BEFORE forwarding to
//     the real DB call, so it still succeeds (ok=true) but takes
//     artificially long to return — simulating a slow-but-successful
//     RenewRunQueueLease (DB contention, GC pause, disk stall).
//   - every renewal attempt AFTER #1 fails outright (simulating a DB outage
//     that starts right after the one slow success).
//
// This combination is what isolates the bug under test: a successful-but-
// slow renewal must leave the SAME absolute lease_expires_at in the
// watchdog's bookkeeping as what was actually committed to the DB, with no
// later successful renewal around to paper over a wrong value by
// overwriting it again. Without the "everything after #1 fails" half, the
// renewal loop's normal TTL/3 cadence keeps re-renewing successfully every
// tick, and each fresh (fast) success overwrites whatever attempt #1 left
// behind before the watchdog's window has a chance to matter — the bug
// would never be observable from outside the process.
type delayedRenewalService struct {
	session.Service
	mu                sync.Mutex
	delayOnAttemptNum int64 // delay exactly this renewal attempt (1-indexed); 0 = never
	delay             time.Duration
	renewalsAttempted atomic.Int64
}

func (s *delayedRenewalService) RenewRunQueueLease(ctx context.Context, id, pumpInstanceID string, newExpiresAt int64) (bool, error) {
	attemptNum := s.renewalsAttempted.Add(1)

	s.mu.Lock()
	delayOnAttemptNum := s.delayOnAttemptNum
	delay := s.delay
	s.mu.Unlock()

	if delayOnAttemptNum > 0 && attemptNum == delayOnAttemptNum {
		// Simulate a slow DB round-trip that nonetheless completes and
		// succeeds well within the caller's context deadline.
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
		return s.Service.RenewRunQueueLease(ctx, id, pumpInstanceID, newExpiresAt)
	}

	if attemptNum > delayOnAttemptNum {
		// Every attempt after the one delayed success fails outright,
		// simulating a DB outage starting right after that one slow
		// success. This freezes the tracked absolute deadline at exactly
		// what attempt #1 left behind, so the watchdog's later decision is
		// attributable ONLY to that one renewal — nothing else can
		// overwrite it before the watchdog's own check fires.
		return false, sql.ErrConnDone
	}

	return s.Service.RenewRunQueueLease(ctx, id, pumpInstanceID, newExpiresAt)
}

// TestP0_3_WatchdogUsesAbsoluteDBExpiryNotPostCallTime verifies that the
// watchdog cancels execution BEFORE the row's real, DB-committed
// lease_expires_at — even when a successful renewal call is slow enough that
// "time since the call returned" would understate the row's true age by more
// than the safety margin.
//
// Scenario (see arithmetic below; TTL=12s, margin=2s, renewDelay=4s):
//   - Renewal ticks roughly every TTL/3 = 4s.
//   - Renewal attempt #1 (tick at ~4s) is delayed by 4s before hitting the
//     real DB — double the 2s safety margin, and comfortably inside the DB
//     call's own context timeout (computed from the remaining safe lease
//     budget at that tick, ~6s) — so it still succeeds (returns at ~8s)
//     instead of erroring out with a context deadline.
//   - Every renewal attempt AFTER #1 fails outright (simulated DB outage),
//     so no later successful renewal can overwrite what attempt #1 left
//     behind — the tracked deadline stays frozen at exactly attempt #1's
//     result for the rest of the test, isolating its effect.
//   - The real DB row's lease_expires_at after attempt #1 is
//     tickStart + TTL = 4s + 12s = 16s from test start, because newExpiresAt
//     is computed BEFORE the delay/DB call, not after.
//   - With the OLD (buggy) post-call-time watchdog, the tracked deadline
//     would have been computed from time.Now() AFTER the 4s delay:
//     (tickStart + delay) + TTL = 8s + 12s = 20s, giving a fire time of
//     20s - 2s = 18s — AFTER the real DB expiry (16s). That is exactly the
//     unsafe case P0-3 closes: another pump could legitimately reclaim the
//     row via CleanupExpiredLeases and start a duplicate execution up to 2s
//     before the old watchdog would even think it needs to stop.
//   - With the FIXED watchdog, the tracked deadline is tickStart + TTL =
//     4s + 12s = 16s, firing at 16s - 2s = 14s — safely (2s nominal) BEFORE
//     the real DB expiry (16s).
func TestP0_3_WatchdogUsesAbsoluteDBExpiryNotPostCallTime(t *testing.T) {
	t.Parallel()

	sess, svc, sqlDB := setupTestSessionWithDB(t, "test-session-p0-3-absolute-expiry")
	ctx := t.Context()

	idempotencyKey := "p0-3-absolute-expiry-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	coord := newContextAwareCoordinator()
	blockCh := make(chan struct{})
	coord.mu.Lock()
	coord.blockCh = blockCh
	coord.mu.Unlock()

	const (
		ttl          = 12 * time.Second
		safetyMargin = 2 * time.Second
		// renewal tick interval is ttl/3 = 4s in the pump's own logic.
		// renewDelay (4s) is 2x the safety margin (2s) and fits comfortably
		// inside the ~6s DB-call context budget available at the first tick,
		// so the delayed call still succeeds rather than timing out.
		renewDelay = 4 * time.Second
	)

	delayedSvc := &delayedRenewalService{
		Service:           svc,
		delayOnAttemptNum: 1,
		delay:             renewDelay,
	}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:                      delayedSvc,
		Coordinator:                   coord,
		PumpInstanceID:                "p0-3-absolute-expiry-pump",
		TestTick:                      func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:                  ttl,
		TestLeaseWatchdogSafetyMargin: safetyMargin,
	})

	testStart := time.Now()
	pump.Start()

	require.Eventually(t, func() bool {
		return coord.entryCount.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)

	// Wait for the (delayed) first renewal to actually complete successfully.
	require.Eventually(t, func() bool {
		return delayedSvc.renewalsAttempted.Load() >= 1
	}, 5*time.Second, 20*time.Millisecond)

	// Wait for the watchdog to fire (execution canceled) or the outer
	// timeout, whichever comes first.
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer timeoutCancel()

	var cancelElapsed time.Duration
	select {
	case <-coord.canceledCh:
		cancelElapsed = time.Since(testStart)
		t.Logf("execution canceled at %v after test start", cancelElapsed)
	case <-timeoutCtx.Done():
		t.Fatal("coordinator did not observe context cancellation within 25s - watchdog did not fire")
	}

	require.True(t, coord.ctxCanceled.Load(), "coordinator should have observed ctx.Done()")

	// Read the row's REAL, DB-committed lease_expires_at directly — this is
	// ground truth for when the lease actually expires, independent of any
	// in-process watchdog bookkeeping.
	var leaseExpiresAtUnix sql.NullInt64
	require.NoError(t, sqlDB.QueryRow(
		`SELECT lease_expires_at FROM session_run_queue WHERE id = ?`, idempotencyKey,
	).Scan(&leaseExpiresAtUnix))

	pump.Stop()
	close(blockCh)

	// If lease_expires_at is already NULL by the time we read it, that is
	// itself direct, even stronger evidence of the exact bug this test
	// guards against: it means the row was ALREADY reclaimed by
	// CleanupExpiredLeases (reset back to pending) before this executor's
	// watchdog ever fired — precisely the double-execution setup P0-3
	// exists to prevent. Fail loudly with that diagnosis rather than a
	// generic "expected non-null" message.
	require.True(t, leaseExpiresAtUnix.Valid,
		"lease_expires_at is NULL: the row was already reclaimed by CleanupExpiredLeases "+
			"(and could have been dispatched to a duplicate execution) BEFORE this executor's "+
			"watchdog fired at %v — the watchdog fired too late relative to the real DB expiry",
		cancelElapsed)

	dbExpiryElapsed := time.Unix(leaseExpiresAtUnix.Int64, 0).Sub(testStart)
	t.Logf("real DB lease_expires_at is %v after test start", dbExpiryElapsed)

	// THE KEY INVARIANT (P0-3): the watchdog must cancel execution BEFORE
	// the row's real, DB-committed expiry — never after or at it. A
	// cancellation at or after dbExpiryElapsed means another pump instance
	// could legitimately have already reclaimed this row via
	// CleanupExpiredLeases and started a duplicate execution before this
	// executor even noticed it should stop. This is the assertion that
	// fails under the pre-fix behavior (revert check confirms this below).
	require.Less(t, cancelElapsed, dbExpiryElapsed,
		"watchdog fired at %v, which is NOT before the real DB lease_expires_at (%v) — duplicate-execution window reopened",
		cancelElapsed, dbExpiryElapsed)

	// Generous sanity bounds (not the core invariant, just confirms the
	// watchdog fired in the right ballpark rather than by accident): should
	// land roughly TTL-margin after the renewal tick, i.e. well after the
	// naive "cancel almost immediately" case and well before the buggy
	// post-call-time deadline (~18s) would have fired.
	require.Greater(t, cancelElapsed, 5*time.Second,
		"watchdog fired suspiciously early (%v)", cancelElapsed)
	require.Less(t, cancelElapsed, 17*time.Second,
		"watchdog fired suspiciously late (%v) — close to or past the buggy post-call-time deadline (~18s)", cancelElapsed)
}
