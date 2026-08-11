package session

// P2-1 regression test (found in the final @oh review of tasks #337-349,
// 2026-08-10, marked PLAUSIBLE — a genuine race between two concurrent
// pump instances, not reproduced live but confirmed by code inspection):
// processEntry's max-attempts-exhaustion branch leases "the oldest pending
// entry for this session" (LeaseRunQueueEntry does NOT take a specific
// entry ID), then unconditionally terminal-fails (deletes) whatever it got
// leased. If another pump instance had already consumed the
// attempts-exhausted entry between this pump's scan and its own lease
// attempt, THIS lease can land on a completely different, healthy,
// never-executed entry for the same session (e.g. a fresh call queued
// moments after the scan) — and that entry would be silently deleted
// without ever running.
//
// This test exercises processEntry directly (this file lives in package
// session, not session_test, specifically to reach the unexported method)
// with a deliberately mismatched entry ID, deterministically forcing the
// race outcome instead of trying to time two real concurrent pumps against
// each other.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's processEntry, remove the `if leased.ID !=
//     entry.ID { ...; return }` guard (added for this fix) so the code
//     falls through directly to TerminalFailRunQueueEntry(leased.ID)
//     unconditionally, as it did before.
//  2. Run: go test ./internal/session -run TestReleaseGate_P2_1_LeaseMismatchDoesNotDeleteWrongEntry -v
//  3. FAIL: the healthy, never-executed entry is deleted (terminal-failed)
//     even though processEntry was only supposed to be terminal-failing
//     the OTHER (attempts-exhausted) entry.
//  4. Restore the guard and PASS.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestReleaseGate_P2_1_LeaseMismatchDoesNotDeleteWrongEntry(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()

	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	q := db.New(conn)
	svc := NewService(q, conn)

	sess, err := svc.Create(ctx, "p2-1-lease-mismatch-probe")
	require.NoError(t, err)

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "probe"})
	require.NoError(t, err)

	// "exhaustedEntry": the one processEntry's scan believes is
	// attempts-exhausted and is trying to terminal-fail. Simulate it
	// already being gone (consumed by "another pump") by never leasing it
	// via processEntry at all — we just need its ID to differ from
	// whatever LeaseRunQueueEntry actually returns.
	err = svc.EnqueueRunQueueEntry(ctx, "exhausted-entry-id", sess.ID, callData)
	require.NoError(t, err)
	// Lease it directly (bypassing processEntry) to simulate another pump
	// instance having already claimed it — so when OUR processEntry call
	// below leases "the oldest pending for this session", it can only land
	// on the healthy entry created next.
	_, err = svc.LeaseRunQueueEntry(ctx, sess.ID, "other-pump-instance", 30*time.Second)
	require.NoError(t, err)

	// "healthyEntry": fresh, zero attempts, must survive.
	err = svc.EnqueueRunQueueEntry(ctx, "healthy-entry-id", sess.ID, callData)
	require.NoError(t, err)

	pump := NewRunQueuePump(RunQueuePumpConfig{
		Sessions:       svc,
		PumpInstanceID: "p2-1-test-pump",
	})

	// Construct the scanned entry exactly as tick() would have (attempts
	// exhausted, matching the "exhausted-entry-id" row that's actually
	// already leased by "other-pump-instance") and call processEntry
	// directly — deterministically forcing the exact race window instead
	// of hoping two real pumps collide within a few milliseconds.
	scannedEntry := &RunQueueEntry{
		ID:        "exhausted-entry-id",
		SessionID: sess.ID,
		Attempts:  RunQueueMaxAttempts,
	}
	pump.processEntry(ctx, scannedEntry)

	// The healthy entry must still exist — not deleted by a mismatched
	// terminal-fail.
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the healthy, never-executed entry must survive a lease mismatch")
	require.Equal(t, "healthy-entry-id", pending[0].ID)
	require.Equal(t, int64(0), pending[0].Attempts, "the healthy entry must not have picked up an attempt penalty from the mismatched lease/release")
}
