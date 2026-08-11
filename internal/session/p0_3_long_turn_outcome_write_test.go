// P0-3 follow-up regression test, found during the closing review of the
// 2026-08-11 release-readiness round: executeEntry created a single dbCtx
// (30s absolute deadline) at function entry, BEFORE calling
// Coordinator.Run, then reused that same context for the post-Run outcome
// write (Ack/Nack/TerminalFail). Any turn longer than 30s hit an already-
// expired context on its outcome write — the write failed with "context
// deadline exceeded", the row stayed leased/pending instead of being
// deleted, and the next tick re-leased and re-executed the same call. That
// is exactly the duplicate-execution bug P0-3's workerWg join was
// introduced to prevent, reintroduced by the same commit via its own new
// dbCtx.
//
// The fix creates the DB write's context fresh, AFTER Coordinator.Run
// returns, via a per-call newDBCtx() closure — see executeEntry's own doc
// comment. TestDBWriteTimeout lets this test force that budget down to a
// few tens of milliseconds so a "long turn" can be simulated with a short
// sleep instead of a real 30+ second wait.
//
// REVERT CHECK: reverting to a single dbCtx created at function entry (the
// original P0-3 shape) makes this test fail with "ack failed after
// success ... context deadline exceeded" and the row still present in the
// database afterward.

package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// sleepingCoordinator is a minimal Coordinator that sleeps for a fixed
// duration before returning success — long enough to outlive a
// deliberately tiny TestDBWriteTimeout, simulating a real LLM turn that
// outlives the 30s production budget without an actual 30+ second wait.
type sleepingCoordinator struct {
	sleep time.Duration
}

func (c *sleepingCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	select {
	case <-time.After(c.sleep):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var result any = "executed"
	return &result, nil
}

// TestP0_3_LongTurnOutcomeWriteSurvivesDBWriteTimeout verifies that a turn
// whose Coordinator.Run call outlives the per-write DB context budget still
// gets Acked successfully — i.e. the budget is applied to the outcome write
// itself, not to the whole executeEntry call including Run().
func TestP0_3_LongTurnOutcomeWriteSurvivesDBWriteTimeout(t *testing.T) {
	t.Parallel()

	sess, svc, sqlDB := setupTestSessionWithDB(t, "test-session-p0-3-long-turn")
	ctx := t.Context()

	idempotencyKey := "p0-3-long-turn-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, idempotencyKey, sess.ID, callDataJSON))

	// The coordinator's Run() sleeps for 150ms — deliberately longer than
	// the 50ms TestDBWriteTimeout below. Before the fix, dbCtx was created
	// (with this same 50ms budget) BEFORE Run() was called, so it would
	// already be expired by the time the post-Run Ack ran.
	coord := &sleepingCoordinator{sleep: 150 * time.Millisecond}

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:           svc,
		Coordinator:        coord,
		PumpInstanceID:     "p0-3-long-turn-pump",
		TestTick:           func() time.Duration { return 10 * time.Millisecond },
		TestLeaseTTL:       200 * time.Millisecond,
		TestDBWriteTimeout: 50 * time.Millisecond,
	})
	pump.Start()

	// Wait for the entry to be Acked (deleted) — poll for the row's
	// disappearance rather than a fixed sleep, since exact scheduling
	// timing varies.
	require.Eventually(t, func() bool {
		var exists bool
		row := sqlDB.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM session_run_queue WHERE id = ?)", idempotencyKey)
		if err := row.Scan(&exists); err != nil {
			return false
		}
		return !exists
	}, 5*time.Second, 20*time.Millisecond,
		"entry must be Acked (deleted) once the long turn completes, even though "+
			"Run() outlived the per-write DB context budget")

	pump.Stop()
}
