package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeAlwaysBusyCoordinator is a mock agent.Coordinator that always reports
// the session as busy (IsSessionBusy returns true forever). This simulates a
// provider/tool that takes longer than 10s to respond to cancellation, which
// is the race condition P1-6 fixes: rerun should NOT proceed if the session
// is still stopping, because the old owner may still be writing to history.
type fakeAlwaysBusyCoordinator struct {
	runCalled        bool
	runWithOverride  bool
	cancelCalled     bool
	clearQueueCalled bool
}

// Implement the agent.Coordinator interface for the fake coordinator.
func (f *fakeAlwaysBusyCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	f.runCalled = true
	return nil, nil
}

func (f *fakeAlwaysBusyCoordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	f.runWithOverride = true
	return nil, nil
}

func (f *fakeAlwaysBusyCoordinator) Cancel(sessionID string) {
	f.cancelCalled = true
}

func (f *fakeAlwaysBusyCoordinator) CancelAll() (stillBusy bool) {
	return true // Always busy — the P1-6 trigger condition
}

func (f *fakeAlwaysBusyCoordinator) IsSessionBusy(sessionID string) bool {
	return true // Always busy — the P1-6 trigger condition
}

func (f *fakeAlwaysBusyCoordinator) IsBusy() bool {
	return true
}

func (f *fakeAlwaysBusyCoordinator) QueuedPrompts(sessionID string) int {
	return 0
}

func (f *fakeAlwaysBusyCoordinator) QueuedPromptsList(sessionID string) []string {
	return nil
}

func (f *fakeAlwaysBusyCoordinator) ClearQueue(sessionID string) {
	f.clearQueueCalled = true
}

func (f *fakeAlwaysBusyCoordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, large, small *agent.ModelOverride, attachments ...message.Attachment) error {
	return nil
}

func (f *fakeAlwaysBusyCoordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	return message.Message{}, nil
}

func (f *fakeAlwaysBusyCoordinator) Summarize(context.Context, string, *agent.SummarizeSnapshot) error {
	return nil
}

func (f *fakeAlwaysBusyCoordinator) SummarizeQueued(sessionID string) bool {
	return false
}

func (f *fakeAlwaysBusyCoordinator) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	return nil, false
}

func (f *fakeAlwaysBusyCoordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	return agent.SessionAgentCall{}, nil
}

func (f *fakeAlwaysBusyCoordinator) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeAlwaysBusyCoordinator) CancelQueuedSummarize(sessionID string) {}

func (f *fakeAlwaysBusyCoordinator) Model() agent.Model {
	return agent.Model{}
}

func (f *fakeAlwaysBusyCoordinator) UpdateModels(ctx context.Context) error {
	return nil
}

func (f *fakeAlwaysBusyCoordinator) GetSystemPrompt() string {
	return ""
}

func (f *fakeAlwaysBusyCoordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	return "", nil
}

func (f *fakeAlwaysBusyCoordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}

func (f *fakeAlwaysBusyCoordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return nil
}

func (f *fakeAlwaysBusyCoordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
}

func (f *fakeAlwaysBusyCoordinator) SetRunLimits(maxCost float64, maxTokens int64) {}

func (f *fakeAlwaysBusyCoordinator) SetActiveModelRole(role config.SelectedModelType) {}

func (f *fakeAlwaysBusyCoordinator) SetAllowPeakHours(allow bool) {}

func (f *fakeAlwaysBusyCoordinator) SetPersistentMode(persistent bool) {}

func (f *fakeAlwaysBusyCoordinator) ResetAutoResumeCounter(sessionID string) {}

// TestP1_6_Rerun_FailsClosedWhenStillStopping is a regression test for Problem 1:
// web rerun must not corrupt the transcript by proceeding while the old owner
// is still stopping. After the 10s timeout, it must fail closed with an error
// instead of unconditionally deleting messages and launching a new run.
//
// This test FAILS when the fix is removed or bypassed, because the old behavior
// proceeds with deletion and Run() even though IsSessionBusy still returns true
// after the timeout.
func TestP1_6_Rerun_FailsClosedWhenStillStopping(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv,
	// which is incompatible with parallel tests.

	// Set up a test app with in-memory DB, isolated from host config.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	// Install the fake coordinator that always reports busy.
	fakeCoord := &fakeAlwaysBusyCoordinator{}
	a.AgentCoordinator = fakeCoord

	ctx := t.Context()

	// Create a session and a user message to rerun.
	sess, err := a.Sessions.Create(ctx, "p1-6-rerun-test")
	require.NoError(t, err)

	targetMsg, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test prompt for rerun"}},
	})
	require.NoError(t, err)

	// Build a fake hub and client to capture the reply.
	hub := newHub()
	client := newClient(hub, nil) // No real WebSocket connection needed for test
	client.send = make(chan []byte, 10)

	// Build the WSMessage payload.
	payload, err := json.Marshal(RerunMessagePayload{MessageID: targetMsg.ID})
	require.NoError(t, err)

	msg := WSMessage{
		ID:      "test-correlation-id",
		Type:    CmdRerunMessage,
		Payload: payload,
	}

	// Call handleRerunMessage directly (the real code path, not a re-implementation).
	handleRerunMessage(ctx, a, client, msg)

	// The handler runs in its own goroutine in production, but handleRerunMessage
	// itself is synchronous (it spawns goroutines internally only). Wait for it.
	// In the fix path, it returns immediately with an error. In the buggy path,
	// it would sleep 10s in the polling loop before proceeding — we need to wait
	// for that to complete too.

	// Check the reply from the client. The fix path sends EventError with
	// "session still stopping — please retry". The buggy path would either:
	// (a) send EventResponse after successfully deleting and running, or
	// (b) send EventError if Run() itself failed for some other reason.

	select {
	case replyBytes := <-client.send:
		var reply WSMessage
		require.NoError(t, json.Unmarshal(replyBytes, &reply))
		require.Equal(t, msg.ID, reply.ID, "reply should echo the message ID")
		require.Equal(t, EventError, reply.Type, "should return EventError when still busy after timeout")
		require.Contains(t, reply.Error, "still stopping", "error message should mention 'still stopping'")
	case <-ctx.Done():
		t.Fatal("timeout waiting for reply")
	}

	// With the fix, the message MUST still exist in the DB (not deleted).
	gotMsg, err := a.Messages.Get(ctx, targetMsg.ID)
	require.NoError(t, err, "message should not be deleted when session still busy after timeout")
	require.Equal(t, targetMsg.ID, gotMsg.ID, "retrieved message should be the original target message")

	// With the fix, Run/RunWithOverrides MUST NOT have been called.
	require.False(t, fakeCoord.runCalled, "Run should not be called when session still busy after timeout")
	require.False(t, fakeCoord.runWithOverride, "RunWithOverrides should not be called when session still busy after timeout")

	// Cancel should have been called (that's step 1 of rerun, before polling).
	require.True(t, fakeCoord.cancelCalled, "Cancel should be called before polling")
	require.True(t, fakeCoord.clearQueueCalled, "ClearQueue should be called before polling")
}
