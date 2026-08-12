package agent

// REVERT CHECK: This test simulates the OLD behavior where LogicalCallID
// was lost during serialization. If we remove the LogicalCallID field from
// SessionAgentCallData and the serialization/deserialization logic, this test
// will FAIL because the round-trip loses the ID and FromDurableQueue re-enqueue
// creates a duplicate (when not skipped).

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestLogicalCallID_RevertCheck simulates the bug: if LogicalCallID is lost
// during serialization, FromDurableQueue calls would incorrectly re-enqueue
// and create duplicate rows. This test validates that our fix prevents this.
func TestLogicalCallID_RevertCheck(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, "p0-1-logicalcallid-revert-check")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	originalLogicalID := uuid.New().String()
	originalCall := SessionAgentCall{
		SessionID:     sess.ID,
		Prompt:        "test prompt",
		LogicalCallID: originalLogicalID,
	}

	// Simulate the OLD broken behavior: serialize to SessionAgentCallData
	// but DON'T preserve LogicalCallID (as the bug did before the fix)
	callData := ToSessionAgentCallData(originalCall)

	// BEFORE FIX: This would be empty (LogicalCallID was not serialized)
	// AFTER FIX: This is NOT empty (we fixed the serialization)
	if callData.LogicalCallID == "" {
		t.Fatal("FAIL: LogicalCallID is empty after serialization - the fix has been broken or removed")
	}

	// Verify the full round-trip preserves the ID
	rebuiltCall := FromSessionAgentCallData(callData)
	require.Equal(t, originalLogicalID, rebuiltCall.LogicalCallID,
		"LogicalCallID must be preserved through round-trip")

	// Mark as FromDurableQueue
	rebuiltCall.FromDurableQueue = true

	// Count rows before
	pendingBefore, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	initialRows := len(pendingBefore)

	// Restart orphaned - with the fix, this should SKIP re-enqueue
	err = sa.restartOrphanedWithRetry([]SessionAgentCall{rebuiltCall})
	require.NoError(t, err)

	// Verify no new row was created
	pendingAfter, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, initialRows, len(pendingAfter),
		"FromDurableQueue calls must not create new durable rows")
}
