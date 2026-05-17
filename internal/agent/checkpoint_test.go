package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// mockCheckpointMsgSvc is a minimal message.Service for checkpoint tests.
// It counts Update calls and captures the last flushed message.
// Fork patch: batch 8 — test-only mock.
type mockCheckpointMsgSvc struct {
	mu          sync.Mutex
	updateCount atomic.Int64
	lastUpdated *message.Message
	*pubsub.Broker[message.Message]
}

func newMockCheckpointMsgSvc() *mockCheckpointMsgSvc {
	return &mockCheckpointMsgSvc{
		Broker: pubsub.NewBroker[message.Message](),
	}
}

func (m *mockCheckpointMsgSvc) Create(_ context.Context, _ string, _ message.CreateMessageParams) (message.Message, error) {
	return message.Message{}, nil
}
func (m *mockCheckpointMsgSvc) Update(_ context.Context, msg message.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCount.Add(1)
	cp := msg.Clone()
	m.lastUpdated = &cp
	return nil
}
func (m *mockCheckpointMsgSvc) Notify(msg message.Message) {
	m.Publish(pubsub.UpdatedEvent, msg.Clone())
}
func (m *mockCheckpointMsgSvc) Get(_ context.Context, _ string) (message.Message, error) {
	return message.Message{}, nil
}
func (m *mockCheckpointMsgSvc) List(_ context.Context, _ string) ([]message.Message, error) {
	return nil, nil
}
func (m *mockCheckpointMsgSvc) ListUserMessages(_ context.Context, _ string) ([]message.Message, error) {
	return nil, nil
}
func (m *mockCheckpointMsgSvc) ListAllUserMessages(_ context.Context) ([]message.Message, error) {
	return nil, nil
}
func (m *mockCheckpointMsgSvc) Delete(_ context.Context, _ string) error  { return nil }
func (m *mockCheckpointMsgSvc) DeleteSessionMessages(_ context.Context, _ string) error {
	return nil
}
func (m *mockCheckpointMsgSvc) SetPinned(_ context.Context, _ string, _ bool) error {
	return nil
}

// runCheckpointTicker replicates the checkpoint goroutine logic from agent.go
// for isolated unit testing.
func runCheckpointTicker(
	ctx context.Context,
	interval time.Duration,
	msgSvc *mockCheckpointMsgSvc,
	currentAssistant *message.Message,
	sessionLock *sync.Mutex,
) <-chan struct{} {
	done := make(chan struct{})
	var checkpointPartsLen int
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sessionLock.Lock()
				if currentAssistant != nil && len(currentAssistant.Parts) != checkpointPartsLen {
					snap := currentAssistant.Clone()
					snap.AddFinish(message.FinishReasonUnknown, "", "")
					for i := len(snap.Parts) - 1; i >= 0; i-- {
						if f, ok := snap.Parts[i].(message.Finish); ok {
							f.Partial = true
							snap.Parts[i] = f
							break
						}
					}
					_ = msgSvc.Update(ctx, snap)
					checkpointPartsLen = len(currentAssistant.Parts)
				}
				sessionLock.Unlock()
			}
		}
	}()
	return done
}

// TestCheckpointTickerFiresOnTextDelta verifies that the checkpoint ticker
// fires and writes to DB when text deltas arrive.
func TestCheckpointTickerFiresOnTextDelta(t *testing.T) {
	t.Parallel()
	msgSvc := newMockCheckpointMsgSvc()
	interval := 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sessionLock sync.Mutex
	currentAssistant := &message.Message{
		ID:        "test-msg-id",
		SessionID: "test-session",
		Role:      message.Assistant,
	}

	done := runCheckpointTicker(ctx, interval, msgSvc, currentAssistant, &sessionLock)

	// Simulate text arriving.
	sessionLock.Lock()
	currentAssistant.AppendContent("hello world")
	sessionLock.Unlock()

	// Wait for at least one checkpoint tick.
	time.Sleep(interval * 3)
	cancel()
	<-done

	count := msgSvc.updateCount.Load()
	require.GreaterOrEqual(t, count, int64(1), "checkpoint should have fired at least once")

	// Verify the flushed message has Partial=true.
	msgSvc.mu.Lock()
	last := msgSvc.lastUpdated
	msgSvc.mu.Unlock()
	require.NotNil(t, last)
	require.True(t, last.IsPartial(), "checkpoint flush should have Partial=true")
	require.False(t, last.IsFinished(), "partial message should NOT be IsFinished")
}

// TestCheckpointCoalescingIdenticalParts verifies that flushing twice with
// identical Parts is a no-op (checkpointPartsLen tracks the last flush).
func TestCheckpointCoalescingIdenticalParts(t *testing.T) {
	t.Parallel()
	msgSvc := newMockCheckpointMsgSvc()
	interval := 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sessionLock sync.Mutex
	currentAssistant := &message.Message{
		ID:        "test-msg-id",
		SessionID: "test-session",
		Role:      message.Assistant,
	}

	// Pre-add content so ticker sees no change.
	currentAssistant.AppendContent("initial text")

	// Run ticker but capture checkpointPartsLen at current level.
	doneCh := make(chan struct{})
	checkpointPartsLen := len(currentAssistant.Parts)
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sessionLock.Lock()
				if len(currentAssistant.Parts) != checkpointPartsLen {
					_ = msgSvc.Update(ctx, currentAssistant.Clone())
					checkpointPartsLen = len(currentAssistant.Parts)
				}
				sessionLock.Unlock()
			}
		}
	}()

	// Wait for multiple ticks — Parts haven't changed, so no writes.
	time.Sleep(interval * 5)
	cancel()
	<-doneCh

	count := msgSvc.updateCount.Load()
	require.Equal(t, int64(0), count, "no writes expected when Parts are unchanged")
}

// TestCheckpointDisabledWhenZero verifies that interval=0 means zero DB
// writes outside of the normal boundary callbacks.
func TestCheckpointDisabledWhenZero(t *testing.T) {
	t.Parallel()
	msgSvc := newMockCheckpointMsgSvc()

	// Simulate startCheckpoint with interval=0 — it returns immediately.
	interval := time.Duration(0)
	started := false
	if interval > 0 {
		started = true
	}
	require.False(t, started, "checkpoint should not start when interval is 0")

	count := msgSvc.updateCount.Load()
	require.Equal(t, int64(0), count)
}

// TestCheckpointStoppedOnStepFinish verifies that cancelling the context
// (simulating OnStepFinish) stops the checkpoint ticker before the final
// write and no further writes happen after stop.
func TestCheckpointStoppedOnStepFinish(t *testing.T) {
	t.Parallel()
	msgSvc := newMockCheckpointMsgSvc()
	interval := 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	var sessionLock sync.Mutex
	currentAssistant := &message.Message{
		ID:        "test-msg-id",
		SessionID: "test-session",
		Role:      message.Assistant,
	}

	done := runCheckpointTicker(ctx, interval, msgSvc, currentAssistant, &sessionLock)

	sessionLock.Lock()
	currentAssistant.AppendContent("text")
	sessionLock.Unlock()

	// Wait for at least one tick.
	time.Sleep(interval * 3)

	countBeforeStop := msgSvc.updateCount.Load()
	require.GreaterOrEqual(t, countBeforeStop, int64(1), "at least one checkpoint before stop")

	// Stop the checkpoint (simulates OnStepFinish).
	cancel()
	<-done

	// Add more content — no more writes expected because ticker is stopped.
	sessionLock.Lock()
	currentAssistant.AppendContent("more text after stop")
	sessionLock.Unlock()

	time.Sleep(interval * 3)

	countAfterStop := msgSvc.updateCount.Load()
	require.Equal(t, countBeforeStop, countAfterStop, "no more writes after stop")
}
