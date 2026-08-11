package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// fakeIsSessionBusyCoordinator is a mock agent.Coordinator that lets us control
// IsSessionBusy independently of Run/RunWithOverrides for testing the busy=false
// broadcast race (P2-2).
type fakeIsSessionBusyCoordinator struct {
	isBusy       bool // Controls what IsSessionBusy returns.
	runCalled    bool
	summarizeErr error
}

// Implement the agent.Coordinator interface methods we need.
func (f *fakeIsSessionBusyCoordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	f.runCalled = true
	return nil, nil
}

func (f *fakeIsSessionBusyCoordinator) RunWithOverrides(ctx context.Context, sessionID string, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	f.runCalled = true
	return nil, nil
}

func (f *fakeIsSessionBusyCoordinator) IsSessionBusy(sessionID string) bool {
	return f.isBusy
}

func (f *fakeIsSessionBusyCoordinator) IsBusy() bool {
	return f.isBusy
}

func (f *fakeIsSessionBusyCoordinator) QueuedPrompts(sessionID string) int {
	return 0
}

func (f *fakeIsSessionBusyCoordinator) QueuedPromptsList(sessionID string) []string {
	return nil
}

func (f *fakeIsSessionBusyCoordinator) ClearQueue(sessionID string) {
}

func (f *fakeIsSessionBusyCoordinator) InterruptAndSend(ctx context.Context, sessionID string, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) error {
	return nil
}

func (f *fakeIsSessionBusyCoordinator) InjectMessage(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (message.Message, error) {
	return message.Message{}, nil
}

func (f *fakeIsSessionBusyCoordinator) Summarize(ctx context.Context, sessionID string, snapshot *agent.SummarizeSnapshot) error {
	return f.summarizeErr
}

func (f *fakeIsSessionBusyCoordinator) SummarizeQueued(sessionID string) bool {
	return false
}

func (f *fakeIsSessionBusyCoordinator) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	return nil, false
}

func (f *fakeIsSessionBusyCoordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	return agent.SessionAgentCall{}, nil
}

func (f *fakeIsSessionBusyCoordinator) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeIsSessionBusyCoordinator) CancelQueuedSummarize(sessionID string) {
}

func (f *fakeIsSessionBusyCoordinator) Cancel(sessionID string) {
}

func (f *fakeIsSessionBusyCoordinator) CancelAll() (stillBusy bool) {
	return false
}

func (f *fakeIsSessionBusyCoordinator) Model() agent.Model {
	return agent.Model{}
}

func (f *fakeIsSessionBusyCoordinator) UpdateModels(ctx context.Context) error {
	return nil
}

func (f *fakeIsSessionBusyCoordinator) GetSystemPrompt() string {
	return ""
}

func (f *fakeIsSessionBusyCoordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	return "", nil
}

func (f *fakeIsSessionBusyCoordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}

func (f *fakeIsSessionBusyCoordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return nil
}

func (f *fakeIsSessionBusyCoordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
}

func (f *fakeIsSessionBusyCoordinator) SetRunLimits(maxCost float64, maxTokens int64) {}

func (f *fakeIsSessionBusyCoordinator) SetActiveModelRole(role config.SelectedModelType) {}

func (f *fakeIsSessionBusyCoordinator) SetAllowPeakHours(allow bool) {}

func (f *fakeIsSessionBusyCoordinator) SetPersistentMode(persistent bool) {}

func (f *fakeIsSessionBusyCoordinator) SetModels(large, small agent.Model) {}

func (f *fakeIsSessionBusyCoordinator) SetTools(tools []fantasy.AgentTool) {}

func (f *fakeIsSessionBusyCoordinator) SetSystemPrompt(prompt string) {}

func (f *fakeIsSessionBusyCoordinator) SetSystemPromptPrefix(prefix string) {}

func (f *fakeIsSessionBusyCoordinator) ResetAutoResumeCounter(sessionID string) {}

func (f *fakeIsSessionBusyCoordinator) RunNonInteractive(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
	return nil, nil
}

// TestP2_2_ConcurrentSend_DoesNotBroadcastBusyFalseWhileStillOwned tests that
// a second concurrent send to an already-busy session does NOT broadcast busy=false
// immediately after Run returns. Run may return early (queued) while the original
// owner is still active, so busy=false would be incorrect. The fix checks
// IsSessionBusy before broadcasting, reflecting the live mailbox state.
//
// This test FAILS without the fix because handlers.go unconditionally broadcasts
// Busy: false after Run returns, even when IsSessionBusy reports true.
func TestP2_2_ConcurrentSend_DoesNotBroadcastBusyFalseWhileStillOwned(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.

	// Set up a test app with in-memory DB, isolated from host config.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	// Install the fake coordinator that reports busy=true even after Run returns.
	// This simulates the race: the second call's Run returned (queued), but
	// the original owner is still active.
	fakeCoord := &fakeIsSessionBusyCoordinator{
		isBusy: true, // Session is still owned by the original turn.
	}
	a.AgentCoordinator = fakeCoord

	ctx := t.Context()

	// Create a session.
	sess, err := a.Sessions.Create(ctx, "p2-2-busy-test")
	require.NoError(t, err)

	// Add a seed message.
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err)

	// Build a fake hub and client to capture broadcasts.
	hub := newHub()
	go hub.Run(ctx)                  // Run the hub's event loop in a goroutine
	t.Cleanup(func() { ctx.Done() }) // Ensure hub stops via context cancellation

	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	// Build the WSMessage payload for handleSendMessage.
	payload, err := json.Marshal(SendMessagePayload{
		SessionID: sess.ID,
		Content:   "second send while busy",
	})
	require.NoError(t, err)

	msg := WSMessage{
		ID:      "test-correlation-id",
		Type:    CmdSendMessage,
		Payload: payload,
	}

	// Call handleSendMessage directly (the real code path, not a re-implementation).
	handleSendMessage(ctx, a, client, msg)

	// Check what was sent from the client. We expect EventAgentBusy broadcasts,
	// but NOT EventAgentBusy{Busy: false} while IsSessionBusy returns true.
	// Give a small delay for the hub's event loop to process broadcasts.
	time.Sleep(100 * time.Millisecond)

	sentEvents := make([]WSMessage, 0)
drainLoop:
	for {
		select {
		case sentBytes := <-client.send:
			var sent WSMessage
			require.NoError(t, json.Unmarshal(sentBytes, &sent))
			sentEvents = append(sentEvents, sent)
		default:
			break drainLoop
		}
	}

	// Find all AgentBusy events and check their Busy states.
	busyFalseBroadcastFound := false
	for _, event := range sentEvents {
		if event.Type == EventAgentBusy {
			var payload AgentBusyPayload
			if err := json.Unmarshal(event.Payload, &payload); err == nil {
				if payload.SessionID == sess.ID && !payload.Busy {
					busyFalseBroadcastFound = true
					break
				}
			}
		}
	}

	// The fix: with the IsSessionBusy check, we should NOT broadcast Busy: false
	// while the session is actually busy. Without the fix, Busy: false would be
	// broadcast unconditionally after Run returns.
	require.False(t, busyFalseBroadcastFound, "should NOT broadcast Busy: false while IsSessionBusy returns true")

	// Verify that Run was actually called (to show the handler executed real code).
	require.True(t, fakeCoord.runCalled, "Run should have been called")

	// Verify that IsSessionBusy was actually checked (to show the handler respected it).
	require.True(t, fakeCoord.IsSessionBusy(sess.ID), "IsSessionBusy should still report true")
}

// TestP2_2_ConcurrentSend_BroadcastsBusyFalseWhenActuallyIdle tests the other
// side of the fix: when the session is actually idle (no owner), we DO broadcast
// busy=false. This ensures we didn't break the normal case.
//
// This test is structured to FAIL without the fix: it first tests that busy=false
// is NOT broadcast when IsSessionBusy=true, then tests that busy=false IS broadcast
// when IsSessionBusy=false. Without the fix, both cases broadcast busy=false,
// so the first check fails.
func TestP2_2_ConcurrentSend_BroadcastsBusyFalseWhenActuallyIdle(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.

	// Set up a test app with in-memory DB, isolated from host config.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	ctx := t.Context()

	// Create a session.
	sess, err := a.Sessions.Create(ctx, "p2-2-idle-test")
	require.NoError(t, err)

	// Add a seed message.
	_, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err)

	// Build a fake hub and client to capture broadcasts.
	hub := newHub()
	go hub.Run(ctx)                  // Run the hub's event loop in a goroutine
	t.Cleanup(func() { ctx.Done() }) // Ensure hub stops via context cancellation

	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	// Helper function to drain and check broadcasts.
	checkBroadcasts := func(fakeCoord *fakeIsSessionBusyCoordinator) bool {
		// Build the WSMessage payload for handleSendMessage.
		payload, err := json.Marshal(SendMessagePayload{
			SessionID: sess.ID,
			Content:   "test message",
		})
		require.NoError(t, err)

		msg := WSMessage{
			ID:      "test-correlation-id",
			Type:    CmdSendMessage,
			Payload: payload,
		}

		// Clear the send channel from previous iterations.
	drainLoop:
		for {
			select {
			case <-client.send:
			default:
				break drainLoop
			}
		}

		// Call handleSendMessage directly (the real code path, not a re-implementation).
		handleSendMessage(ctx, a, client, msg)

		// Check what was sent from the client.
		// Give a small delay for the hub's event loop to process broadcasts.
		time.Sleep(100 * time.Millisecond)

		busyFalseBroadcastFound := false
	drainCheck:
		for {
			select {
			case sentBytes := <-client.send:
				var sent WSMessage
				require.NoError(t, json.Unmarshal(sentBytes, &sent))
				if sent.Type == EventAgentBusy {
					var payload AgentBusyPayload
					if err := json.Unmarshal(sent.Payload, &payload); err == nil {
						if payload.SessionID == sess.ID && !payload.Busy {
							busyFalseBroadcastFound = true
							break drainCheck
						}
					}
				}
			default:
				break drainCheck
			}
		}

		return busyFalseBroadcastFound
	}

	// First test: with IsSessionBusy=true, busy=false should NOT be broadcast.
	fakeCoordBusy := &fakeIsSessionBusyCoordinator{isBusy: true}
	a.AgentCoordinator = fakeCoordBusy

	busyFalseWhenBusy := checkBroadcasts(fakeCoordBusy)
	require.False(t, busyFalseWhenBusy, "should NOT broadcast Busy: false when IsSessionBusy returns true")
	require.True(t, fakeCoordBusy.IsSessionBusy(sess.ID), "IsSessionBusy should report true")

	// Second test: with IsSessionBusy=false, busy=false SHOULD be broadcast.
	fakeCoordIdle := &fakeIsSessionBusyCoordinator{isBusy: false}
	a.AgentCoordinator = fakeCoordIdle

	busyFalseWhenIdle := checkBroadcasts(fakeCoordIdle)
	require.True(t, busyFalseWhenIdle, "should broadcast Busy: false when IsSessionBusy returns false")
	require.False(t, fakeCoordIdle.IsSessionBusy(sess.ID), "IsSessionBusy should report false")

	// Without the fix, the first check fails because busy=false is broadcast
	// unconditionally regardless of IsSessionBusy.
}
