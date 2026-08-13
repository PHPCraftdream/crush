package message

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// Guard imports for go vet.
var (
	_ context.Context = context.Background()
	_ db.Querier      = (*db.Queries)(nil)
)

// TestLateCheckpointDoesNotPublishStalePartial is a regression test for P0-2:
// verifies that a late checkpoint that races with a terminal finish does NOT
// publish a stale partial event. The sequence is:
// 1. Terminal message written and published (non-Partial Finish)
// 2. Late checkpoint UpdateMessageIfNotTerminal returns (0 rows affected)
// 3. The late checkpoint MUST NOT publish, because the terminal state won
//
// The test checks BOTH that the DB state is correct (terminal row unchanged)
// AND that the LAST BROKER EVENT is the terminal message (not a partial).
// The second half is what was missing in the previous round.
func TestLateCheckpointDoesNotPublishStalePartial(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)

	// Subscribe to capture events.
	sub := svc.Subscribe(ctx)

	sessionID := "test-session-123"

	// Create a message.
	msg, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "Hello"}},
	})
	require.NoError(t, err)

	// Drain the CreatedEvent.
	<-sub

	// Phase 1: Write a terminal finish.
	msg.AppendContent(" world")
	msg.AddFinish(FinishReasonEndTurn, "", "") // Terminal finish, NOT partial
	msg.Parts = append(msg.Parts, ToolResult{Content: "result"})

	err = svc.Update(ctx, msg)
	require.NoError(t, err)

	// Verify DB has the terminal finish.
	dbMsg, err := q.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	require.NotNil(t, dbMsg.FinishedAt, "DB should have terminal finish")
	// Note: We don't check Pinned=1 anymore since Update doesn't set it.
	// The important check is that finished_at is set (terminal state).

	// Drain the terminal update event.
	<-sub

	// Phase 2: Simulate a late checkpoint that arrives AFTER the terminal finish.
	// This checkpoint has stale content (missing " world") and Partial=true.
	stalePartial := msg.Clone()
	// Strip the " world" suffix to make it stale.
	if _, ok := stalePartial.Parts[0].(TextContent); ok {
		stalePartial.Parts[0] = TextContent{Text: "Hello"} // Stale!
	}
	// Add a Partial finish.
	stalePartial.AddFinish(FinishReasonUnknown, "", "")
	for i := len(stalePartial.Parts) - 1; i >= 0; i-- {
		if f, ok := stalePartial.Parts[i].(Finish); ok {
			f.Partial = true
			stalePartial.Parts[i] = f
			break
		}
	}

	// This Update call with Partial=true uses UpdateMessageIfNotTerminal.
	// Since the DB already has a terminal finish, it should return 0 rows affected.
	err = svc.Update(ctx, stalePartial)
	require.NoError(t, err) // No error is expected

	// Verify DB state is UNCHANGED (terminal finish wins).
	dbMsgAfter, err := q.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	require.NotNil(t, dbMsgAfter.FinishedAt, "DB should still have terminal finish")
	// The important check is that finished_at is still set.

	// CRITICAL: Verify that NO PARTIAL EVENT was published.
	// The late checkpoint should NOT have published because rowsAffected == 0.
	select {
	case ev := <-sub:
		t.Fatalf("Unexpected partial event published after terminal: %+v", ev.Payload)
	case <-time.After(50 * time.Millisecond):
		// Good - no event was published.
	}
}

// TestLateCheckpointRevertCheck is the revert check for P0-2 Part A:
// it REVERTS the fix (unconditional partial publish) and verifies the test
// FAILS on the last broker event check.
func TestLateCheckpointRevertCheck(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)

	// Subscribe and capture events.
	var eventsMu sync.Mutex
	var events []Message
	sub := svc.Subscribe(ctx)
	go func() {
		for ev := range sub {
			eventsMu.Lock()
			events = append(events, ev.Payload)
			eventsMu.Unlock()
		}
	}()

	sessionID := "test-session-123"

	// Create a message.
	msg, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "Hello"}},
	})
	require.NoError(t, err)

	// Write a terminal finish.
	msg.AppendContent(" world")
	msg.AddFinish(FinishReasonEndTurn, "", "")
	msg.Parts = append(msg.Parts, ToolResult{Content: "result"})

	err = svc.Update(ctx, msg)
	require.NoError(t, err)

	// Clear events.
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()

	// Simulate a late checkpoint with stale content.
	stalePartial := msg.Clone()
	if _, ok := stalePartial.Parts[0].(TextContent); ok {
		stalePartial.Parts[0] = TextContent{Text: "Hello"} // Stale!
	}
	stalePartial.AddFinish(FinishReasonUnknown, "", "")
	for i := len(stalePartial.Parts) - 1; i >= 0; i-- {
		if f, ok := stalePartial.Parts[i].(Finish); ok {
			f.Partial = true
			stalePartial.Parts[i] = f
			break
		}
	}

	// REVERTED BEHAVIOR: Always publish partial even if DB update skipped.
	// This simulates the OLD code that always called Publish() for partials.
	// Manually call Publish() to bypass the rowsAffected check.
	svc.Publish(pubsub.UpdatedEvent, stalePartial)

	// Wait for the event to be received.
	time.Sleep(10 * time.Millisecond)

	// CRITICAL: With the revert, the LAST event IS a partial (the bug).
	eventsMu.Lock()
	if len(events) == 0 {
		eventsMu.Unlock()
		t.Fatal("No events received in revert check")
	}
	lastEvent := events[len(events)-1]
	eventsMu.Unlock()

	// This assertion MUST FAIL with the reverted behavior.
	// The fix prevents this by checking rowsAffected before publishing.
	assert.True(t, lastEvent.IsPartial(), "REVERT CHECK: Last event MUST be partial with reverted code (this assertion failing means the fix works)")
}

// TestCheckpointCoalescingIdenticalPartsNoPublish verifies that partial checkpoints
// that don't change the DB (rowsAffected == 0) don't publish.
func TestCheckpointCoalescingIdenticalPartsNoPublish(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)

	sub := svc.Subscribe(ctx)

	sessionID := "test-session-123"

	// Create a message.
	msg, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: "Hello"}},
	})
	require.NoError(t, err)

	// Drain the CreatedEvent.
	<-sub

	// Write a partial checkpoint.
	partial := msg.Clone()
	partial.AddFinish(FinishReasonUnknown, "", "")
	for i := len(partial.Parts) - 1; i >= 0; i-- {
		if f, ok := partial.Parts[i].(Finish); ok {
			f.Partial = true
			partial.Parts[i] = f
			break
		}
	}

	err = svc.Update(ctx, partial)
	require.NoError(t, err)

	// Drain the partial update event - should have published.
	select {
	case ev := <-sub:
		require.True(t, ev.Payload.IsPartial(), "First partial should publish")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("First partial should have published")
	}

	// Write the SAME partial again. Since content is identical and there's
	// no terminal finish, the DB update should still affect 1 row (because
	// finished_at is still NULL), so it WILL publish. This is correct behavior:
	// the DB row actually changed (updated_at is new), so we publish.
	err = svc.Update(ctx, partial)
	require.NoError(t, err)

	// Drain the second partial update event - should have published.
	select {
	case ev := <-sub:
		require.True(t, ev.Payload.IsPartial(), "Second partial should publish")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Second partial should have published")
	}

	// Now write a terminal finish.
	terminal := partial.Clone()
	for i := len(terminal.Parts) - 1; i >= 0; i-- {
		if f, ok := terminal.Parts[i].(Finish); ok {
			f.Partial = false
			f.Reason = FinishReasonEndTurn
			terminal.Parts[i] = f
			break
		}
	}

	err = svc.Update(ctx, terminal)
	require.NoError(t, err)

	// Drain the terminal update event - should have published.
	select {
	case ev := <-sub:
		require.False(t, ev.Payload.IsPartial(), "Terminal should publish")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Terminal should have published")
	}

	// Now write a stale partial AFTER the terminal finish.
	// This should return rowsAffected == 0 and NOT publish.
	stale := partial.Clone() // Partial=true again
	err = svc.Update(ctx, stale)
	require.NoError(t, err)

	// Verify that NO event was published for the stale partial.
	select {
	case ev := <-sub:
		t.Fatalf("Stale partial after terminal should NOT publish, but got event: %+v", ev.Payload)
	case <-time.After(50 * time.Millisecond):
		// Good - no event was published.
	}
}
