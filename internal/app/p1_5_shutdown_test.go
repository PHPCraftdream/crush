package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"charm.land/fantasy"
	agent "github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func (m *mockCoordinatorForShutdown) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	panic("unexpected: Shutdown does not call BuildSystemPromptForSession")
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

// TestP1_5_ForcedShutdownPolicy_SkipsDBCloseWhenStillBusy proves the forced-shutdown
// policy: when CancelAll returns stillBusy=true, Shutdown skips DB close (to avoid
// corrupting live writers) and returns within bounded time. The observable effect
// is that the DB remains usable after forced shutdown (db.Release was NOT called).
//
// NOTE: This test verifies the POLICY effect (skip DB close), not the warning log.
// The warning itself is best-effort telemetry; the critical invariant is that we
// don't close the DB under live writers.
func TestP1_5_ForcedShutdownPolicy_SkipsDBCloseWhenStillBusy(t *testing.T) {
	t.Run("Shutdown skips DB close when CancelAll reports stillBusy=true", func(t *testing.T) {
		// Use a real temporary data directory and real db.Connect.
		dataDir := t.TempDir()

		// Connect to DB first - this creates the pool entry.
		entry, err := db.Connect(context.Background(), dataDir)
		require.NoError(t, err, "failed to connect to DB")
		require.NotNil(t, entry, "entry should not be nil")

		mockCoord := &mockCoordinatorForShutdown{
			cancelAllFunc: func() (stillBusy bool) {
				return true // Simulate agents still busy after grace period
			},
		}

		app := &App{
			AgentCoordinator: mockCoord,
			DB:               func() *sql.DB { return nil }, // Not used in this test
			dataDir:          dataDir,
			dbReleasesNeeded: 1, // One Connect call
			globalCtx:        context.Background(),
		}

		// Call Shutdown. With the forced-shutdown policy, it should:
		// 1. Get stillBusy=true from CancelAll
		// 2. Skip DB close (not call db.Release)
		// 3. Return within bounded time
		shutdownStart := time.Now()

		done := make(chan struct{})
		go func() {
			app.Shutdown()
			close(done)
		}()

		select {
		case <-done:
			// Shutdown completed (expected with forced-shutdown policy).
		case <-time.After(12 * time.Second):
			t.Fatal("Shutdown did not complete within 12 seconds")
		}

		shutdownDuration := time.Since(shutdownStart)

		// Verify that Shutdown returned quickly (not blocked by busy agents).
		assert.Less(t, shutdownDuration, 12*time.Second,
			"Shutdown with stillBusy=true should still return within bounded time")

		// CRITICAL: Verify DB was NOT closed because shutdown was forced.
		// We verify this by trying to use the connection - it should still work.
		// Shutdown() skipped Release() (forced-shutdown policy under test), so
		// the entry's refCount is still 1 from the initial Connect above. A new
		// Connect() here increments it to 2 and must return the SAME entry
		// (not create a new one).
		entry2, err := db.Connect(context.Background(), dataDir)
		require.NoError(t, err, "should still be able to connect after forced shutdown")
		require.Same(t, entry, entry2, "should return same entry (DB not released)")

		// Clean up: release BOTH Connect() calls this subtest made.
		require.NoError(t, db.Release(dataDir), "cleanup release should succeed")
		require.NoError(t, db.Release(dataDir), "second cleanup release should succeed")

		t.Logf("Verified forced-shutdown policy: Shutdown returned in %v and DB remains usable (not closed)", shutdownDuration)
	})
}
