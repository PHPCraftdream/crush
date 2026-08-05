package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDescendantSessionIDs_ParallelSubAgents_BothVisible is the regression
// test for release task #297/#300 (commit 55eb2fc5): the live-tail loop used
// to follow only the SINGLE freshest descendant session
// (computeCallTreeActivity's DeepestSessionID), so when two sub-agents were
// delegated in parallel, whichever one wrote second-most-recently was
// completely invisible to `crush sessions watch` — the operator would see
// only one of the two working children, with no indication the other one
// existed at all.
//
// descendantSessionIDs replaces that single-freshest lookup with a real BFS
// over session.ListSubSessions, so the watch loop's per-tick sub-agent tail
// (liveTailSession's `for _, childID := range descendantSessionIDs(...)`
// loop) visits every live child, not just one.
//
// This test drives the REAL descendantSessionIDs against a real DB with two
// sibling children of the same parent (simulating two parallel "agent" tool
// delegations) plus a grandchild under one of them (a sub-agent that itself
// delegates further) — proving both same-level parallelism AND multi-level
// nesting are covered, which a single-freshest lookup could never surface
// regardless of which one happened to be newest.
func TestDescendantSessionIDs_ParallelSubAgents_BothVisible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)

	parent, err := s.Create(ctx, "parent with two parallel sub-agents")
	require.NoError(t, err)

	childA, err := s.CreateTaskSession(ctx, "msg1$$callA", parent.ID, "sub-agent A")
	require.NoError(t, err)
	childB, err := s.CreateTaskSession(ctx, "msg1$$callB", parent.ID, "sub-agent B")
	require.NoError(t, err)
	// A grandchild: sub-agent A itself delegates further. Only reachable by
	// walking MULTIPLE BFS levels, not just rootID's immediate children.
	grandchild, err := s.CreateTaskSession(ctx, "msgA$$callC", childA.ID, "sub-sub-agent")
	require.NoError(t, err)

	a := &app.App{Sessions: s}

	got := descendantSessionIDs(ctx, a, parent.ID)

	assert.ElementsMatch(t, []string{childA.ID, childB.ID, grandchild.ID}, got,
		"descendantSessionIDs must return EVERY descendant — both parallel siblings AND the "+
			"grandchild — not just whichever one a single-freshest lookup would have picked")

	// rootID itself must never appear in its own descendant list.
	assert.NotContains(t, got, parent.ID)
}

// TestDescendantSessionIDs_DepthCapPreventsCycleHang is the regression test
// for descendantSessionIDs' own defensive maxDepth guard: a live-tail loop
// runs on every poll tick for the lifetime of a `sessions watch` invocation,
// so an unbounded walk over corrupt/cyclic parent_session_id data (a
// child accidentally pointing back at an ancestor) would hang the CLI
// solid, defeating the entire point of a live-tail display. This test
// seeds a genuine cycle (impossible via CreateTaskSession's normal API, so
// forced directly through the session service's update path) and asserts
// the walk terminates and returns a bounded, non-exploding result instead
// of hanging or growing forever.
func TestDescendantSessionIDs_DepthCapPreventsCycleHang(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)

	root, err := s.Create(ctx, "cycle root")
	require.NoError(t, err)
	// child's parent is root (CreateTaskSession's normal edge) — left
	// untouched below.
	child, err := s.CreateTaskSession(ctx, "msg$$call", root.ID, "cycle child")
	require.NoError(t, err)

	// Force ROOT's own parent to point at CHILD via a raw UPDATE, forming a
	// genuine mutual cycle: root -> child (via child's PRESERVED
	// parent_session_id = root.ID) and child -> root (via root's NEW
	// parent_session_id = child.ID). With a single-parent-per-row model,
	// this is the only construction where BOTH nodes of the cycle stay
	// reachable starting the walk from root: rewriting a DESCENDANT's
	// parent to point back up (the more "natural"-looking corruption)
	// necessarily severs that same descendant's edge FROM its reachable
	// ancestor, which was tried and confirmed to produce an unreachable
	// (hence untested) cycle instead. descendantSessionIDs must not be able
	// to trust application-level invariants alone — its own maxDepth guard
	// (see its doc comment) is the actual safety net a real "walked back
	// into an ancestor" data corruption would depend on.
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET parent_session_id = ? WHERE id = ?`, child.ID, root.ID)
	require.NoError(t, err)

	a := &app.App{Sessions: s}

	done := make(chan []string, 1)
	go func() {
		done <- descendantSessionIDs(ctx, a, root.ID)
	}()

	select {
	case got := <-done:
		// The walk must terminate (proving the depth cap works) — bounded by
		// maxDepth, NOT exploding into an unbounded/duplicate-heavy result
		// despite the cycle (root and child re-discovering each other on
		// every level would otherwise grow linearly with depth forever).
		assert.LessOrEqual(t, len(got), 16,
			"descendantSessionIDs must respect its own maxDepth cap even against cyclic parent data")
		// Proves the walk did genuine traversal (not an empty/error
		// short-circuit that would vacuously satisfy the bound above).
		assert.Contains(t, got, child.ID, "the walk must still discover the real child before the cycle is hit")
	case <-time.After(5 * time.Second):
		t.Fatal("descendantSessionIDs hung on cyclic session data — its maxDepth guard did not terminate the walk")
	}
}

// TestPrintNewMessagesSince_EventDriven_NoRepeatOnUnchangedSession is the
// regression test for the core "real activity, not a timer pulse" contract
// (commit d0e13893 / 55eb2fc5): the OLD sub-agent display printed a
// "sub-agent active: activity Ns ago" line on every poll tick regardless of
// whether the child session had produced anything new, because the note
// embedded a live-updating age string that made the "only when changed"
// throttle never actually fire. printNewMessagesSince replaces that with a
// real diff against the child session's message history: nothing new means
// nothing printed, full stop — no note, no line, no output at all.
//
// This test calls printNewMessagesSince twice in a row with NO new message
// written to the child session in between (simulating multiple poll ticks
// while a sub-agent is between tool calls) and asserts the second call
// writes NOTHING — the defining behavioral difference from the old
// timer-driven pulse, which would have printed an (aging) note on every
// single tick regardless.
func TestPrintNewMessagesSince_EventDriven_NoRepeatOnUnchangedSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "msg$$call", parent.ID, "sub-agent")
	require.NoError(t, err)

	_, err = m.Create(ctx, child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "doing the task"}},
	})
	require.NoError(t, err)

	a := &app.App{Sessions: s, Messages: m}
	lastPrinted := map[string]string{}

	var buf1 bytes.Buffer
	printed1 := printNewMessagesSince(&buf1, ctx, a, child.ID, lastPrinted, time.Now())
	assert.True(t, printed1, "first call must backfill the child's existing message history")
	assert.NotEmpty(t, buf1.String())

	// Second call, same session, NO new message written — the "quiet tick"
	// every timer-pulse-driven implementation used to still print a line for.
	var buf2 bytes.Buffer
	printed2 := printNewMessagesSince(&buf2, ctx, a, child.ID, lastPrinted, time.Now())
	assert.False(t, printed2,
		"a tick with no new sub-agent activity must print NOTHING — this is the exact defect the fix "+
			"closed: the old timer-driven note re-rendered a live-updating age string every tick, so "+
			"'only print when changed' never actually throttled anything")
	assert.Empty(t, buf2.String(), "no bytes may be written to the writer on a quiet tick")

	// A THIRD call, after genuine new activity, must print again — proving
	// the quiet result above is because nothing changed, not because the
	// function is broken/stuck.
	newMsg, err := m.Create(ctx, child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "found the answer"}},
	})
	require.NoError(t, err)
	// created_at has ONLY second granularity (see message.service.Update's
	// time.Now().Unix() and the messages table's INTEGER column), so within a
	// fast test run this new message can land in the SAME second as the
	// first — isAfter then falls back to comparing random UUIDs
	// (a.ID > b.ID), which is not deterministic. Force created_at strictly
	// past the first message's so the "new activity is detected" assertion
	// below cannot flake depending on wall-clock/UUID luck.
	_, err = conn.ExecContext(ctx, `UPDATE messages SET created_at = created_at + 10 WHERE id = ?`, newMsg.ID)
	require.NoError(t, err)

	var buf3 bytes.Buffer
	printed3 := printNewMessagesSince(&buf3, ctx, a, child.ID, lastPrinted, time.Now())
	assert.True(t, printed3, "genuine new activity after a quiet tick must be printed")
	assert.Contains(t, buf3.String(), "found the answer")
}

// TestSubAgentWriter_PrefixesEveryLine proves the display half of the fix:
// output routed through a child session's writer is visually distinguishable
// from the parent's own stream (every line prefixed with "  [sub-agent
// <hash>] "), which is what lets an operator tell a delegation's live tool
// calls apart from the top-level session's own output in the combined
// stdout stream liveTailSession writes to.
func TestSubAgentWriter_PrefixesEveryLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := subAgentWriter(&buf, "child-session-id")

	n, err := w.Write([]byte("line one\nline two\n"))
	require.NoError(t, err)
	assert.Equal(t, len("line one\nline two\n"), n)

	out := buf.String()
	assert.Contains(t, out, "[sub-agent ")
	lines := splitNonEmptyLines(out)
	require.Len(t, lines, 2)
	for _, line := range lines {
		assert.Contains(t, line, "[sub-agent ", "every line must carry the sub-agent prefix, not just the first")
	}
	assert.Contains(t, lines[0], "line one")
	assert.Contains(t, lines[1], "line two")
}

func splitNonEmptyLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if line := s[start:i]; line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
