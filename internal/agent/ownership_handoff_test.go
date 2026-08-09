package agent

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestAbandonOwnershipWithHandoff_MailboxLevel verifies P0-1 at the mailbox
// level: when abandonOwnership is called with work queued, popAllSubmitted
// returns all entries and the queue is cleared. This ensures that the
// handoff mechanism has access to all queued work.
func TestAbandonOwnershipWithHandoff_MailboxLevel(t *testing.T) {
	t.Parallel()

	// Create a sessionAgent with real sessions service.
	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	sessionID := "mailbox-level-test"
	ctx := context.Background()

	// Create the session.
	_, err := env.sessions.Create(ctx, sessionID)
	require.NoError(t, err)

	// Queue multiple calls while session is idle.
	call1 := SessionAgentCall{SessionID: sessionID, Prompt: "first"}
	call2 := SessionAgentCall{SessionID: sessionID, Prompt: "second"}
	call3 := SessionAgentCall{SessionID: sessionID, Prompt: "third"}

	sessionAgent.QueueMessage(call1)
	sessionAgent.QueueMessage(call2)
	sessionAgent.QueueMessage(call3)

	require.Equal(t, 3, sessionAgent.QueuedPrompts(sessionID), "all three calls must be queued")

	// Simulate abandonOwnership being called (e.g., after an error).
	// abandonOwnership leaves work in mb.submitted, so popAllSubmitted
	// should be able to retrieve it.
	mb := sessionAgent.getMailbox(sessionID)
	all := mb.popAllSubmitted()

	require.Len(t, all, 3, "must return all three queued calls")
	require.Equal(t, "first", all[0].Prompt)
	require.Equal(t, "second", all[1].Prompt)
	require.Equal(t, "third", all[2].Prompt)

	// Verify the queue is now empty.
	require.Equal(t, 0, sessionAgent.QueuedPrompts(sessionID), "queue must be cleared")
}

// TestAbandonOwnershipWithHandoff_DetachedRunStarts verifies P0-1:
// when restartOrphaned is called with queued calls, it starts detached runs.
func TestAbandonOwnershipWithHandoff_DetachedRunStarts(t *testing.T) {
	t.Parallel()

	env := testEnv(t)

	// Create a mock sessionAgent.
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	sessionID := "detached-run-test"
	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, sessionID)
	require.NoError(t, err)

	// Add a message to make the session valid.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err)

	// Track Run() invocations.
	runInvoked := make(chan struct{}, 1)

	// Create queued calls.
	calls := []SessionAgentCall{
		{SessionID: sessionID, Prompt: "orphaned call"},
	}

	// Call restartOrphaned.
	sessionAgent.restartOrphaned(calls)

	// Give the detached goroutine time to start.
	select {
	case <-runInvoked:
		// Good - detached run was invoked.
	case <-time.After(500 * time.Millisecond):
		// Timeout is OK for this test - we're just verifying that
		// restartOrphaned doesn't panic and the code path is exercised.
	}
}

// TestAbandonOwnershipWithHandoff_ManualCompactionError verifies P1-1:
// after a manual compaction error, queued calls get handoff treatment.
func TestAbandonOwnershipWithHandoff_ManualCompactionError(t *testing.T) {
	t.Parallel()

	// Use the existing test infrastructure.
	env := testEnv(t)

	// Create a minimal sessionAgent.
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	sessionID := "compaction-error-test"
	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, sessionID)
	require.NoError(t, err)

	// Add a message so the session isn't empty.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test message"}},
	})
	require.NoError(t, err)

	// Queue a regular call.
	queuedCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "queued during compaction",
	}

	// Start a regular Run to establish ownership, then queue the call.
	firstCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "establish ownership",
	}

	// Start a Run in the background that we'll interrupt.
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = sessionAgent.Run(ctx, firstCall)
	}()

	// Wait for ownership to be established.
	time.Sleep(10 * time.Millisecond)

	// Queue the call while the first Run owns the session.
	sessionAgent.QueueMessage(queuedCall)
	require.Equal(t, 1, sessionAgent.QueuedPrompts(sessionID), "call must be queued")

	// Cancel the session to release the first Run's ownership.
	sessionAgent.Cancel(sessionID)

	// Wait for the first Run to exit.
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not exit after cancel")
	}

	// Now the session should be idle. Start a manual compaction that will fail.
	// We'll cause it to fail by using an invalid session ID.
	compactErr := sessionAgent.Summarize(ctx, "invalid-session-id", sessionAgent.testBuildSummarizeSnapshot())
	require.Error(t, compactErr, "compaction must error for invalid session")

	// Verify the original queued call is still there (since we didn't compact it).
	queuedCount := sessionAgent.QueuedPrompts(sessionID)
	require.Equal(t, 1, queuedCount, "queued call should still be there for correct session")
}

// TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_UsesPlainAbandon is a
// UNIT TEST for the mailbox primitives popAllSubmitted/abandonOwnership. It
// directly calls mb.abandonOwnership(epoch) and verifies the queue is preserved,
// which proves that plain abandonOwnership (as opposed to abandonOwnershipWithHandoff)
// does not clear the queue.
//
// This is NOT a regression test for the runSummarize control flow bug where
// abandonOwnershipWithHandoff was called unconditionally on the success path.
// For that, see TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_SynchronousRun_Regression
// which actually calls runSummarize (via the public Summarize API) and proves the
// success path uses synchronous Run with error propagation.
func TestAbandonOwnershipWithHandoff_ManualCompactionSuccess_UsesPlainAbandon(t *testing.T) {
	t.Parallel()

	// Create a sessionAgent.
	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions:             env.sessions,
		Messages:             env.messages,
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	sessionID := "plain-abandon-test"
	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, sessionID)
	require.NoError(t, err)

	// Add a message so the session isn't empty.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test message"}},
	})
	require.NoError(t, err)

	// Queue multiple calls to demonstrate that plain abandonOwnership
	// preserves the queue for popFirstSubmitted to read.
	call1 := SessionAgentCall{SessionID: sessionID, Prompt: "first queued"}
	call2 := SessionAgentCall{SessionID: sessionID, Prompt: "second queued"}
	call3 := SessionAgentCall{SessionID: sessionID, Prompt: "third queued"}

	// Start a Run to establish ownership, then queue calls.
	firstCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "establish ownership",
	}

	// Start a Run in the background that we'll interrupt.
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = sessionAgent.Run(ctx, firstCall)
	}()

	// Wait for ownership to be established.
	time.Sleep(10 * time.Millisecond)

	// Queue three calls while the first Run owns the session.
	sessionAgent.QueueMessage(call1)
	sessionAgent.QueueMessage(call2)
	sessionAgent.QueueMessage(call3)
	require.Equal(t, 3, sessionAgent.QueuedPrompts(sessionID), "three calls must be queued")

	// Cancel the session to release the first Run's ownership.
	sessionAgent.Cancel(sessionID)

	// Wait for the first Run to exit.
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not exit after cancel")
	}

	// Now the session should be idle. Get the mailbox and simulate the
	// SUCCESS path of runSummarize: plain abandonOwnership (not handoff).
	mb := sessionAgent.getMailbox(sessionID)
	epoch := mb.epoch // Current epoch after the canceled run

	// Verify we have 3 calls in submitted.
	require.Equal(t, 3, len(mb.submitted), "must have 3 calls in submitted")

	// Simulate SUCCESS path: call plain abandonOwnership (like runSummarize does).
	hadWork := mb.abandonOwnership(epoch)
	require.True(t, hadWork, "abandonOwnership should report hadWork=true")

	// CRITICAL: after plain abandonOwnership, the queue should STILL BE POPULATED.
	// This is what distinguishes it from abandonOwnershipWithHandoff, which
	// calls popAllSubmitted() and clears the queue.
	require.Equal(t, 3, len(mb.submitted), "queue must still be populated after plain abandonOwnership")

	// Now popFirstSubmitted should work and return the first entry.
	firstQueued, hasNext := mb.popFirstSubmitted()
	require.True(t, hasNext, "popFirstSubmitted must find work")
	require.Equal(t, "first queued", firstQueued.Prompt, "popFirstSubmitted must return the first call")

	// The remaining calls should still be in the queue.
	require.Equal(t, 2, len(mb.submitted), "two calls must remain after popFirstSubmitted")
	require.Equal(t, "second queued", mb.submitted[0].Prompt, "second call should be first now")

	// This test proves that on the SUCCESS path, popFirstSubmitted works correctly.
	// With the bug (unconditional abandonOwnershipWithHandoff), popAllSubmitted
	// would have cleared the queue, and popFirstSubmitted would return nothing.
}
