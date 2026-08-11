package session

// P0-3 regression test (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// closes a real "Add concurrently with Wait" race an earlier version of the
// P0-3 fix left in processEntry. That version checked p.ctx.Err() and then
// called p.workerWg.Add(1) as two separate, unsynchronized steps — between
// the check and the Add, a concurrent Stop() could cancel p.ctx and spin up
// its workerWg.Wait() goroutine while the counter was still 0, letting
// Wait() return (nothing registered yet) a moment before the Add(1) landed.
// Per the sync.WaitGroup contract ("calls with a positive delta that start
// when the counter is zero must happen before a Wait"), this either panics
// at runtime or lets a brand-new worker start after Stop() has already told
// its caller (App.Shutdown) that shutdown was safe — reintroducing the
// exact class of bug P0-3 exists to close, one level down. The fix gates
// both the check and the Add under one mutex (admitMu/stopping).
//
// This test exercises processEntry directly (package session, not
// session_test, specifically to reach the unexported method and fields) by
// flipping the gate manually — deterministic, rather than trying to time a
// real Stop() call against a real dispatch within a few microseconds.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's processEntry, replace the admitMu-guarded
//     `if p.stopping { ...release...; return }` / `p.workerWg.Add(1)` block
//     with the unsynchronized version: `if p.ctx.Err() != nil { return };
//     p.workerWg.Add(1)` (checking p.ctx.Err() instead of p.stopping, no
//     admitMu lock).
//  2. Run: go test ./internal/session -run TestP0_3_AdmissionGateRejectsAfterStopping -v
//  3. FAIL: this test sets pump.stopping = true directly (never cancels
//     pump.ctx), so the reverted p.ctx.Err() check sees a live context and
//     falls through to executeEntry — the entry gets ACKED (deleted), not
//     released back to pending. require.Len(t, pendingAfter, 1) fails.
//  4. Restore the admitMu/stopping gate and this test passes again.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// noopCoordinator is a Coordinator that would succeed if ever called; this
// test's whole point is that the admission gate prevents it from ever being
// called, so its Run body only needs to satisfy the interface.
type noopCoordinator struct{}

func (noopCoordinator) Run(ctx context.Context, call SessionAgentCallData) (*any, error) {
	var result any = "should not have been called"
	return &result, nil
}

func TestP0_3_AdmissionGateRejectsAfterStopping(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()

	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })

	q := db.New(conn)
	svc := NewService(q, conn)

	sess, err := svc.Create(ctx, "p0-3-admission-gate-probe")
	require.NoError(t, err)

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "probe"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p0-3-gate-key", sess.ID, callData))

	pump := NewRunQueuePump(RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    noopCoordinator{},
		PumpInstanceID: "p0-3-gate-test-pump",
	})

	// Simulate Stop() having already flipped the gate, WITHOUT calling the
	// real Stop() (which would also cancel pump.ctx and spin up its own
	// Wait goroutine) — isolates the admission-gate behavior on its own.
	pump.admitMu.Lock()
	pump.stopping = true
	pump.admitMu.Unlock()

	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	entry := pending[0]

	pump.processEntry(ctx, &entry)

	// The entry must be released back to pending (Nack, no attempt
	// penalty) — not executed (which would Ack/delete it) and not left
	// stuck in "leased" limbo for a full leaseTTL.
	pendingAfter, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingAfter, 1, "entry must be released back to pending, not executed or left leased")
	require.Equal(t, entry.ID, pendingAfter[0].ID)
	require.Equal(t, int64(0), pendingAfter[0].Attempts, "release-on-shutdown must not count as an attempt")

	// inFlight must not retain the session, or this pump instance would
	// treat the session as permanently busy (it was marked in-flight
	// before the admission gate ran, and only executeEntry's defer — which
	// never runs for a rejected worker — normally clears it).
	pump.inFlightMu.Lock()
	_, stillInFlight := pump.inFlight[sess.ID]
	pump.inFlightMu.Unlock()
	require.False(t, stillInFlight, "inFlight must be cleared when a worker is rejected by the admission gate")
}
