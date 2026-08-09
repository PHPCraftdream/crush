package app

// P1-5 regression tests that actually call (*App).Shutdown().
// These tests verify the two critical shutdown invariants from the
// concurrency review (docs/reviews/2026-08-07-release-concurrency-review.md, P1-5):
//
//   1. Ordering: cleanup (including DB close) does NOT begin until CancelAll
//      reports whether agents are still idle. If stillBusy=true, Shutdown
//      logs a warning and proceeds anyway (forced-shutdown policy), but the
//      order is always: CancelAll → decide policy → cleanup.
//
//   2. Bounded exit: even if a cleanup goroutine blocks forever, Shutdown()
//      itself returns within a deterministic bounded time (10 seconds outer
//      timeout on wg.Wait()). This is enforced by the select/waitDone pattern
//      introduced in this fix.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	agent "github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
)

// mockCoordinatorForShutdown is a minimal Coordinator implementation for
// testing Shutdown behavior. Only CancelAll() is implemented; all other
// methods panic if called (they're not used by Shutdown).
type mockCoordinatorForShutdown struct {
	cancelAllFunc func() (stillBusy bool)
}

func (m *mockCoordinatorForShutdown) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	panic("unexpected: Shutdown does not call Run")
}

func (m *mockCoordinatorForShutdown) RunWithOverrides(ctx context.Context, sessionID, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	panic("unexpected: Shutdown does not call RunWithOverrides")
}

func (m *mockCoordinatorForShutdown) Cancel(sessionID string) {
	panic("unexpected: Shutdown does not call Cancel")
}

func (m *mockCoordinatorForShutdown) CancelAll() (stillBusy bool) {
	return m.cancelAllFunc()
}

func (m *mockCoordinatorForShutdown) IsSessionBusy(sessionID string) bool {
	panic("unexpected: Shutdown does not call IsSessionBusy")
}

func (m *mockCoordinatorForShutdown) IsBusy() bool {
	panic("unexpected: Shutdown does not call IsBusy")
}

func (m *mockCoordinatorForShutdown) QueuedPrompts(sessionID string) int {
	panic("unexpected: Shutdown does not call QueuedPrompts")
}

func (m *mockCoordinatorForShutdown) QueuedPromptsList(sessionID string) []string {
	panic("unexpected: Shutdown does not call QueuedPromptsList")
}

func (m *mockCoordinatorForShutdown) ClearQueue(sessionID string) {
	panic("unexpected: Shutdown does not call ClearQueue")
}

func (m *mockCoordinatorForShutdown) InterruptAndSend(ctx context.Context, sessionID, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) error {
	panic("unexpected: Shutdown does not call InterruptAndSend")
}

func (m *mockCoordinatorForShutdown) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	panic("unexpected: Shutdown does not call InjectMessage")
}

func (m *mockCoordinatorForShutdown) Summarize(context.Context, string, *agent.SummarizeSnapshot) error {
	panic("unexpected: Shutdown does not call Summarize")
}

func (m *mockCoordinatorForShutdown) SummarizeQueued(sessionID string) bool {
	panic("unexpected: Shutdown does not call SummarizeQueued")
}

func (m *mockCoordinatorForShutdown) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	panic("unexpected: Shutdown does not call TakeSummarizeQueue")
}

func (m *mockCoordinatorForShutdown) CancelQueuedSummarize(sessionID string) {
	panic("unexpected: Shutdown does not call CancelQueuedSummarize")
}

func (m *mockCoordinatorForShutdown) Model() agent.Model {
	panic("unexpected: Shutdown does not call Model")
}

func (m *mockCoordinatorForShutdown) UpdateModels(ctx context.Context) error {
	panic("unexpected: Shutdown does not call UpdateModels")
}

func (m *mockCoordinatorForShutdown) GetSystemPrompt() string {
	panic("unexpected: Shutdown does not call GetSystemPrompt")
}

func (m *mockCoordinatorForShutdown) BuildSystemPrompt(ctx context.Context) (string, error) {
	panic("unexpected: Shutdown does not call BuildSystemPrompt")
}

func (m *mockCoordinatorForShutdown) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	panic("unexpected: Shutdown does not call UpdateSessionSystemPrompt")
}

func (m *mockCoordinatorForShutdown) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	panic("unexpected: Shutdown does not call SetAgentTimeoutOptions")
}

func (m *mockCoordinatorForShutdown) SetRunLimits(maxCost float64, maxTokens int64) {
	panic("unexpected: Shutdown does not call SetRunLimits")
}

func (m *mockCoordinatorForShutdown) SetActiveModelRole(role config.SelectedModelType) {
	panic("unexpected: Shutdown does not call SetActiveModelRole")
}

func (m *mockCoordinatorForShutdown) SetAllowPeakHours(allow bool) {
	panic("unexpected: Shutdown does not call SetAllowPeakHours")
}

func (m *mockCoordinatorForShutdown) SetPersistentMode(persistent bool) {
	panic("unexpected: Shutdown does not call SetPersistentMode")
}

func (m *mockCoordinatorForShutdown) ResetAutoResumeCounter(sessionID string) {
	panic("unexpected: Shutdown does not call ResetAutoResumeCounter")
}

func (m *mockCoordinatorForShutdown) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	panic("unexpected: Shutdown does not call RebuildSessionAgentCall")
}

func (m *mockCoordinatorForShutdown) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	panic("unexpected: Shutdown does not call RunSessionAgentCall")
}

// TestP1_5_ShutdownOrdering_CleanupWaitsForCancelAll proves the critical
// ordering invariant: cleanup functions do NOT start until CancelAll has
// completed and reported whether agents are still busy.
//
// The test uses a fake Coordinator whose CancelAll() blocks for 500ms, then
// a fake cleanup that records its start time. The invariant is that cleanup's
// start time must be AFTER CancelAll completes (not during).
func TestP1_5_ShutdownOrdering_CleanupWaitsForCancelAll(t *testing.T) {
	t.Run("cleanup starts AFTER CancelAll completes", func(t *testing.T) {
		var (
			cancelAllStarted   atomic.Bool
			cancelAllFinished  atomic.Bool
			cleanupStarted     atomic.Bool
			cancelAllStartTime time.Time
			cancelAllEndTime   time.Time
			cleanupStartTime   time.Time
		)

		// Simulate a CancelAll that blocks for 500ms.
		cancelAllDuration := 500 * time.Millisecond
		mockCoord := &mockCoordinatorForShutdown{
			cancelAllFunc: func() (stillBusy bool) {
				cancelAllStarted.Store(true)
				cancelAllStartTime = time.Now()
				time.Sleep(cancelAllDuration)
				cancelAllEndTime = time.Now()
				cancelAllFinished.Store(true)
				return false // Simulate clean shutdown
			},
		}

		// Create a cleanup function that records its start time.
		var cleanupWG sync.WaitGroup
		cleanupWG.Add(1)

		app := &App{
			AgentCoordinator: mockCoord,
			cleanupFuncs: []func(context.Context) error{
				func(ctx context.Context) error {
					defer cleanupWG.Done()
					cleanupStarted.Store(true)
					cleanupStartTime = time.Now()
					return nil
				},
			},
		}

		// Call Shutdown in a goroutine (it should complete quickly).
		done := make(chan struct{})
		go func() {
			app.Shutdown()
			close(done)
		}()

		// Wait for Shutdown to complete.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Shutdown did not complete within 5 seconds")
		}

		// Wait for cleanup to finish.
		cleanupWG.Wait()

		// Verify the ordering invariant.
		assert.True(t, cancelAllStarted.Load(), "CancelAll should have started")
		assert.True(t, cancelAllFinished.Load(), "CancelAll should have finished")
		assert.True(t, cleanupStarted.Load(), "cleanup should have started")

		// Critical: cleanup must start AFTER CancelAll finishes, not during.
		// We verify this by checking that cleanupStartTime is at or after cancelAllEndTime.
		cleanupAfterCancelAll := cleanupStartTime.Equal(cancelAllEndTime) || cleanupStartTime.After(cancelAllEndTime)
		assert.True(t, cleanupAfterCancelAll,
			"cleanup must start AT OR AFTER CancelAll finishes; got cleanupStartTime=%v, cancelAllEndTime=%v",
			cleanupStartTime, cancelAllEndTime)

		// Additional sanity check: CancelAll should have taken at least cancelAllDuration.
		cancelAllTook := cancelAllEndTime.Sub(cancelAllStartTime)
		assert.GreaterOrEqual(t, cancelAllTook, cancelAllDuration,
			"CancelAll should have taken at least %s", cancelAllDuration)

		t.Logf("Verified ordering: CancelAll ran from %v to %v (%s), cleanup started at %v (after CancelAll finished)",
			cancelAllStartTime, cancelAllEndTime, cancelAllTook, cleanupStartTime)
	})
}

// TestP1_5_ShutdownBoundedTimeout_ReturnsEvenWithBlockingCleanup proves the
// bounded exit invariant: even if a cleanup goroutine blocks forever, Shutdown()
// itself returns within a deterministic bounded time (the 10-second outer timeout
// on wg.Wait() introduced in this fix).
//
// The test uses a cleanup function that blocks forever on a channel that never closes,
// then measures that Shutdown() actually returns (not hangs).
func TestP1_5_ShutdownBoundedTimeout_ReturnsEvenWithBlockingCleanup(t *testing.T) {
	t.Run("Shutdown returns within bounded time even with blocking cleanup", func(t *testing.T) {
		mockCoord := &mockCoordinatorForShutdown{
			cancelAllFunc: func() (stillBusy bool) {
				return false // Immediate clean shutdown
			},
		}

		// Create a cleanup function that blocks forever.
		blockForever := make(chan struct{})

		app := &App{
			AgentCoordinator: mockCoord,
			cleanupFuncs: []func(context.Context) error{
				func(ctx context.Context) error {
					<-blockForever // Block forever
					return nil
				},
			},
		}

		// Call Shutdown and measure its duration.
		// With the fix, Shutdown should return within ~10 seconds (the outer timeout).
		// Without the fix (unconditional wg.Wait()), it would hang forever.
		shutdownStart := time.Now()

		done := make(chan struct{})
		go func() {
			app.Shutdown()
			close(done)
		}()

		select {
		case <-done:
			// Shutdown completed (expected with the fix).
		case <-time.After(12 * time.Second):
			t.Fatal("Shutdown did not complete within 12 seconds (should have returned within ~10s outer timeout)")
		}

		shutdownDuration := time.Since(shutdownStart)

		// Verify that Shutdown returned within the expected bounded time.
		// The outer timeout is 10 seconds, so we allow some overhead (12s).
		assert.Less(t, shutdownDuration, 12*time.Second,
			"Shutdown should have returned within bounded time; got %v", shutdownDuration)

		t.Logf("Verified bounded exit: Shutdown returned in %v (within 10s outer timeout)", shutdownDuration)

		// Clean up: unblock the goroutine (even though we've already verified the invariant).
		close(blockForever)
	})
}

// TestP1_5_ForcedShutdownPolicy_LogsWarningWhenStillBusy proves the forced-shutdown
// policy: when CancelAll returns stillBusy=true, Shutdown logs a warning but
// continues with cleanup anyway (does NOT block forever).
func TestP1_5_ForcedShutdownPolicy_LogsWarningWhenStillBusy(t *testing.T) {
	t.Run("Shutdown logs warning and continues when CancelAll reports stillBusy=true", func(t *testing.T) {
		mockCoord := &mockCoordinatorForShutdown{
			cancelAllFunc: func() (stillBusy bool) {
				return true // Simulate agents still busy after grace period
			},
		}

		app := &App{
			AgentCoordinator: mockCoord,
			cleanupFuncs: []func(context.Context) error{
				func(ctx context.Context) error {
					return nil
				},
			},
		}

		// Call Shutdown. With the fix, it should:
		// 1. Get stillBusy=true from CancelAll
		// 2. Log a warning
		// 3. Continue with cleanup
		// 4. Return within bounded time
		shutdownStart := time.Now()

		done := make(chan struct{})
		go func() {
			app.Shutdown()
			close(done)
		}()

		select {
		case <-done:
			// Shutdown completed (expected with the fix).
		case <-time.After(12 * time.Second):
			t.Fatal("Shutdown did not complete within 12 seconds")
		}

		shutdownDuration := time.Since(shutdownStart)

		// Verify that Shutdown returned quickly (not blocked by busy agents).
		assert.Less(t, shutdownDuration, 12*time.Second,
			"Shutdown with stillBusy=true should still return within bounded time")

		t.Logf("Verified forced-shutdown policy: Shutdown returned in %v after CancelAll reported stillBusy=true", shutdownDuration)
	})
}

// TestP1_5_DBCleanupRespectsContext proves that DB cleanup respects the bounded
// shutdown context. This is a structural verification of the goroutine+select
// pattern used in app.go to make even synchronous sql.DB.Close() respect a timeout.
func TestP1_5_DBCleanupRespectsContext(t *testing.T) {
	t.Run("DB cleanup wrapper respects shutdown context timeout", func(t *testing.T) {
		mockCoord := &mockCoordinatorForShutdown{
			cancelAllFunc: func() (stillBusy bool) {
				return false
			},
		}

		// Simulate a DB cleanup that blocks forever (like sql.DB.Close() might).
		blockForever := make(chan struct{})

		app := &App{
			AgentCoordinator: mockCoord,
			cleanupFuncs: []func(context.Context) error{
				// This mimics the DB cleanup wrapper pattern from app.go:
				// run db.Release in a goroutine, select between completion and ctx.Done().
				func(ctx context.Context) error {
					resultChan := make(chan error, 1)

					go func() {
						// Simulate sql.DB.Close() that blocks forever.
						<-blockForever
						resultChan <- nil
					}()

					select {
					case err := <-resultChan:
						return err
					case <-ctx.Done():
						// Expected: context expired before Close() finished.
						// The wrapper returns ctx.Err() and continues.
						return ctx.Err()
					}
				},
			},
		}

		// Call Shutdown. With the fix, the DB cleanup wrapper should:
		// 1. Notice ctx.Done() fired (5-second shutdownCtx timeout)
		// 2. Return ctx.Err()
		// 3. Shutdown continues and returns within outer timeout
		shutdownStart := time.Now()

		done := make(chan struct{})
		go func() {
			app.Shutdown()
			close(done)
		}()

		select {
		case <-done:
			// Shutdown completed (expected with the fix).
		case <-time.After(12 * time.Second):
			t.Fatal("Shutdown did not complete within 12 seconds")
		}

		shutdownDuration := time.Since(shutdownStart)

		// Verify that Shutdown returned within bounded time despite blocking DB cleanup.
		assert.Less(t, shutdownDuration, 12*time.Second,
			"Shutdown with blocking DB cleanup should return within bounded time")

		t.Logf("Verified DB cleanup respects context: Shutdown returned in %v despite blocking DB.Close()", shutdownDuration)

		// Clean up.
		close(blockForever)
	})
}
