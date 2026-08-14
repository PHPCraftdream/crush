package session_test

// Focused unit coverage for RunQueuePump.DrainSessionNow (task #421/P0-1),
// complementing the slower, full end-to-end regression test in
// internal/app (TestRunNonInteractive_P0_1_LiveContinuation, which exercises
// the real interrupt ticker + RunNonInteractive wiring but necessarily waits
// out the real 3s interruptInjectTick). These tests isolate DrainSessionNow's
// own three observable outcomes directly against a real RunQueuePump/DB,
// without needing a real interrupt or a real Coordinator.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDrainSessionNow_NothingPending verifies the no-op contract:
// DrainSessionNow must not treat "nothing pending" as having recovered
// anything (drained=false, err=nil). RunNonInteractive relies on this to
// leave a genuine user/--timeout cancellation (no durable continuation)
// alone instead of fabricating a success.
func TestDrainSessionNow_NothingPending(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "drain-now-nothing-pending")
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &mockCoordinator{},
		PumpInstanceID: "test-pump-drain-empty",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drained, err := pump.DrainSessionNow(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, drained, "nothing was pending — must not report anything drained")
}

// TestDrainSessionNow_ExecutesPendingEntry verifies the core contract: a
// durably enqueued entry for the session is leased, executed, and acked
// synchronously by DrainSessionNow itself — without waiting for a
// background tick — exactly the property RunNonInteractive depends on to
// finish a cross-process interrupt's continuation before the process exits.
func TestDrainSessionNow_ExecutesPendingEntry(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "drain-now-executes")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-drain-exec",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-now-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drained, err := pump.DrainSessionNow(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, drained)
	require.Equal(t, int64(1), coord.calls.Load(), "the pending entry must have been executed exactly once")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "the executed entry must be acked/removed, not left pending")
}

// TestDrainSessionNow_WaitsForConcurrentBackgroundTick verifies the
// race-closing path documented on DrainSessionNow: if this SAME pump
// instance's own background tick wins the lease race for the pending entry
// (fires concurrently with — and just ahead of — DrainSessionNow's own
// attempt), DrainSessionNow must wait for that execution to finish and
// still report drained=true, rather than concluding "nothing to drain"
// just because ITS OWN lease attempt found the row already taken by a
// goroutine it didn't start.
func TestDrainSessionNow_WaitsForConcurrentBackgroundTick(t *testing.T) {
	sess, svc := setupTestSession(t, "drain-now-race")
	coord := &slowCoordinatorForDrain{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-drain-race",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})
	pump.Start()
	t.Cleanup(func() { pump.Stop() })

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-now-race-idem-1", sess.ID, callData))

	// Wait for the BACKGROUND tick (not this test's later DrainSessionNow
	// call) to win the lease and start executing — confirms the race this
	// test targets is actually set up before DrainSessionNow ever runs.
	select {
	case <-coord.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background tick never started executing — test setup is broken, proves nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var drained bool
	var drainErr error
	go func() {
		defer close(drainDone)
		drained, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Give DrainSessionNow time to find nothing pending (already leased by
	// the background tick) and enter its inFlight poll loop before
	// releasing the background execution.
	time.Sleep(50 * time.Millisecond)
	close(coord.release)

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainSessionNow did not return after the concurrent background execution finished")
	}

	require.NoError(t, drainErr)
	require.True(t, drained, "DrainSessionNow must report drained=true when the background tick executed the entry, even though its own lease attempt found nothing pending")
	require.Equal(t, int64(1), coord.calls.Load(), "the entry must have executed exactly once (by the background tick), not a second time via DrainSessionNow")
}

type countingCoordinatorForDrain struct {
	calls atomic.Int64
}

func (c *countingCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, nil
}

type slowCoordinatorForDrain struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (c *slowCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}
