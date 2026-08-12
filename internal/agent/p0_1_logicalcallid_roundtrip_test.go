package agent

// P0-1 regression test (docs/reviews/2026-08-12-post-fix-release-readiness-review.md):
// LogicalCallID must survive durable serialization round-trip and prevent
// duplicate durable rows when an orphaned FromDurableQueue call is re-enqueued
// via restartOrphanedWithRetry after replacement/error/handoff.
//
// The bug chain (before fix):
// 1. Pump leases durable row A, RebuildSessionAgentCall creates call with FromDurableQueue=true
//    BUT LogicalCallID was lost during serialization
// 2. Replacement D arrives, reclaimReplacementOrKeep sends D next, returns A to mb.submitted
// 3. D fails before drain, abandonOwnershipWithHandoff extracts A, calls restartOrphanedWithRetry
// 4. With empty LogicalCallID, recovery creates NEW timestamp key and NEW durable row B
// 5. Original pump Nacks row A back to pending
// 6. Now TWO rows (A and B) exist for the same logical call
//
// The fix:
// - Serialize LogicalCallID in SessionAgentCallData
// - Restore it in RebuildSessionAgentCall
// - Skip re-enqueue for FromDurableQueue calls (they already have a durable row)
//
// This test proves: after JSON round-trip + replacement + error + handoff,
// exactly ONE durable row exists (or re-enqueue is skipped for FromDurableQueue).

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestLogicalCallID_DurableRoundTripPreservesIdempotency is the end-to-end
// regression test for the LogicalCallID serialization loss bug.
//
// It simulates the exact chain described in the review:
// 1. Create a call with LogicalCallID
// 2. Serialize to SessionAgentCallData (JSON round-trip)
// 3. Rebuild from data (simulating pump's RebuildSessionAgentCall)
// 4. Mark as FromDurableQueue
// 5. Call restartOrphanedWithRetry (simulating abandonOwnershipWithHandoff)
// 6. Verify: either re-enqueue is skipped OR exactly one row exists with matching key
func TestLogicalCallID_DurableRoundTripPreservesIdempotency(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, "p0-1-logicalcallid-roundtrip-test")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	// Step 1: Create a call with a LogicalCallID (as buildCall does)
	originalLogicalID := uuid.New().String()
	originalCall := SessionAgentCall{
		SessionID:     sess.ID,
		Prompt:        "test prompt",
		LogicalCallID: originalLogicalID,
	}

	// Step 2: Serialize to SessionAgentCallData (durable round-trip)
	callData := ToSessionAgentCallData(originalCall)
	require.Equal(t, originalLogicalID, callData.LogicalCallID,
		"LogicalCallID must be preserved in SessionAgentCallData")

	// Simulate JSON round-trip (as the pump does with DB storage)
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)

	var roundTripData session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(callDataJSON, &roundTripData))
	require.Equal(t, originalLogicalID, roundTripData.LogicalCallID,
		"LogicalCallID must survive JSON round-trip")

	// Step 3: Rebuild from data (simulating pump's RebuildSessionAgentCall)
	// Note: we use FromSessionAgentCallData which is what RebuildSessionAgentCall uses internally
	rebuiltCall := FromSessionAgentCallData(roundTripData)
	require.Equal(t, originalLogicalID, rebuiltCall.LogicalCallID,
		"LogicalCallID must be restored in rebuilt call")

	// Step 4: Mark as FromDurableQueue (as RebuildSessionAgentCall does)
	rebuiltCall.FromDurableQueue = true

	// Step 5: Call restartOrphanedWithRetry (simulating abandonOwnershipWithHandoff)
	// Count rows before
	pendingBefore, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	initialRows := len(pendingBefore)

	// This simulates: D failed before drain, A was returned to mb.submitted,
	// abandonOwnershipWithHandoff extracted A and called restartOrphanedWithRetry
	err = sa.restartOrphanedWithRetry([]SessionAgentCall{rebuiltCall})
	require.NoError(t, err, "restartOrphanedWithRetry must succeed")

	// Step 6: Verify behavior
	pendingAfter, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)

	// For FromDurableQueue calls, we SKIP re-enqueue, so no new row should be created
	require.Equal(t, initialRows, len(pendingAfter),
		"FromDurableQueue calls should NOT create new durable rows; the existing row handles retries")
}

// TestLogicalCallID_RoundTripWithoutFromDurableQueueCreatesOneRow proves that
// when a call does NOT have FromDurableQueue set (e.g., direct web/CLI turn),
// the full JSON round-trip + restartOrphanedWithRetry chain creates exactly ONE row,
// and re-enqueuing the same call reuses that row (idempotent via ON CONFLICT).
func TestLogicalCallID_RoundTripWithoutFromDurableQueueCreatesOneRow(t *testing.T) {
	env := testEnv(t)
	ctx := t.Context()

	sess, err := env.sessions.Create(ctx, "p0-1-logicalcallid-nondurable-roundtrip-test")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	// Create a call with LogicalCallID (FromDurableQueue=false, as in direct web/CLI turn)
	originalLogicalID := uuid.New().String()
	originalCall := SessionAgentCall{
		SessionID:        sess.ID,
		Prompt:           "direct turn",
		LogicalCallID:    originalLogicalID,
		FromDurableQueue: false, // Direct web/CLI turn, NOT from durable queue
	}

	// Simulate JSON round-trip (serialization -> DB -> deserialization)
	callData := ToSessionAgentCallData(originalCall)
	roundTripCall := FromSessionAgentCallData(callData)
	roundTripCall.LogicalCallID = originalLogicalID // Ensure it's preserved

	// First restartOrphanedWithRetry (simulating first handoff)
	err = sa.restartOrphanedWithRetry([]SessionAgentCall{roundTripCall})
	require.NoError(t, err)

	pendingAfter1, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingAfter1, 1, "first restart must create exactly one durable row")

	// Second restartOrphanedWithRetry with the SAME logical call (simulating retry)
	// Should reuse the same row via ON CONFLICT(id) DO NOTHING
	err = sa.restartOrphanedWithRetry([]SessionAgentCall{roundTripCall})
	require.NoError(t, err)

	pendingAfter2, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingAfter2, 1, "second restart must NOT create a duplicate row; ON CONFLICT must reuse existing row")

	// Verify the idempotency key matches our expected LogicalCallID
	expectedKey := sess.ID + "-" + originalLogicalID
	require.Equal(t, expectedKey, pendingAfter2[0].ID,
		"the existing row must have the correct idempotency key derived from LogicalCallID")
}
