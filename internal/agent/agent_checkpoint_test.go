package agent

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestCheckpointFencing_P0_4Regression verifies that a hung checkpoint
// cannot overwrite a terminal finish. This is a unit test that focuses
// on the conditional DB update mechanism (UpdateMessageIfNotTerminal).
func TestCheckpointFencing_P0_4Regression(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbDir := t.TempDir()
	conn, err := db.Connect(ctx, dbDir)
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	t.Cleanup(func() {
		conn.Close()
	})

	testSession, err := sessions.Create(ctx, "Test Session")
	require.NoError(t, err)

	// Create an assistant message
	assistantMsg, err := messages.Create(ctx, testSession.ID, message.CreateMessageParams{
		Role:                "assistant",
		Model:               "test-model",
		Provider:            "test-provider",
		ReasoningEffort:     "",
		IsSummaryMessage:    false,
		Hidden:              false,
		AutoResumed:         false,
		BackgroundJobNotice: false,
	})
	require.NoError(t, err)

	// Simulate a checkpoint write with Partial finish
	checkpointSnap := assistantMsg
	checkpointSnap.Parts = append(checkpointSnap.Parts, message.TextContent{Text: "checkpoint snapshot"})
	checkpointSnap.AddFinish(message.FinishReasonUnknown, "", "")
	// Mark as partial (this is what the checkpoint goroutine does)
	for i := len(checkpointSnap.Parts) - 1; i >= 0; i-- {
		if f, ok := checkpointSnap.Parts[i].(message.Finish); ok {
			f.Partial = true
			checkpointSnap.Parts[i] = f
			break
		}
	}

	// Write the checkpoint
	err = messages.Update(ctx, checkpointSnap)
	require.NoError(t, err, "Checkpoint write should succeed")

	// Verify checkpoint was written (finished_at should be NULL)
	readAfterCheckpoint, err := messages.Get(ctx, assistantMsg.ID)
	require.NoError(t, err)
	finishPart := readAfterCheckpoint.FinishPart()
	require.NotNil(t, finishPart, "Should have a finish part")
	require.True(t, finishPart.Partial, "Checkpoint should be partial")

	// Now write a terminal finish (simulating OnStepFinish's final write)
	terminalSnap := assistantMsg
	terminalSnap.Parts = append(terminalSnap.Parts, message.TextContent{Text: "terminal state"})
	terminalSnap.AddFinish(message.FinishReasonEndTurn, "", "")
	// NOT partial - this is a real finish

	err = messages.Update(ctx, terminalSnap)
	require.NoError(t, err, "Terminal write should succeed")

	// Verify terminal was written
	readAfterTerminal, err := messages.Get(ctx, assistantMsg.ID)
	require.NoError(t, err)
	terminalFinish := readAfterTerminal.FinishPart()
	require.NotNil(t, terminalFinish, "Should have a finish part")
	require.False(t, terminalFinish.Partial, "Terminal should not be partial")
	require.Greater(t, terminalFinish.Time, int64(0), "finish time should be positive")

	// Now simulate the hung checkpoint unblocking and trying to write again
	// This is the core P0-4 scenario: a stale partial checkpoint trying to
	// overwrite a terminal finish.
	hungCheckpoint := checkpointSnap // Same partial snapshot from before

	err = messages.Update(ctx, hungCheckpoint)
	require.NoError(t, err, "Hung checkpoint write should not error (it's just skipped)")

	// Verify the terminal state was NOT overwritten
	finalRead, err := messages.Get(ctx, assistantMsg.ID)
	require.NoError(t, err)

	// The key invariant: the finish should still be non-partial
	finalFinish := finalRead.FinishPart()
	require.NotNil(t, finalFinish, "Should have a finish part")
	require.False(t, finalFinish.Partial, "Terminal finish should remain non-partial after stale checkpoint attempt")

	// And the parts should reflect the terminal state, not the stale checkpoint
	require.Contains(t, finalRead.FullText(), "terminal state", "Terminal parts should persist")
}

// TestCheckpointContextCancellation_P0_4Regression: intentionally removed.
// The original version of this test had an empty body (comments describing
// the fencing mechanism "by code inspection", zero assertions or calls) —
// it asserted nothing and would pass identically whether or not the P0-4
// fix existed. Real, non-vacuous coverage for the same mechanism lives in
// TestP0_4_StopCheckpointCancelsBlockedWrite (p0_4_checkpoint_cancel_test.go
// / p0_4_shutdown_join_test.go), which genuinely blocks a checkpoint write,
// calls CancelAll, and asserts cancellation is observed within milliseconds
// rather than the 5s grace.
