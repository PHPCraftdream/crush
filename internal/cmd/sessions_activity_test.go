package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestComputeCallTreeActivity_SubAgentFresher builds a parent session with an
// old message and a child (sub-agent) session with a newer message, and
// asserts the call-tree walk reports the child as the freshest activity and
// flags SubAgentActive. This is the core of the whole feature: the fresh edge
// of work lives on the CHILD session's rows, invisible to a plain
// Messages.List on the parent.
func TestComputeCallTreeActivity_SubAgentFresher(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(ctx, "parent")
	require.NoError(t, err)

	// Parent's own message (older). We can't set created_at directly, so we
	// create the parent message first, then the child message after, and rely
	// on the child ending up with a >= timestamp. To make the ordering robust
	// against same-second timestamps, we assert the DEEPEST session is the
	// child (SubAgentActive) which holds regardless of exact tie-breaking as
	// long as the child ts >= parent ts.
	_, err = m.Create(ctx, parent.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "delegate please"}},
	})
	require.NoError(t, err)

	child, err := s.CreateTaskSession(ctx, "msg$$call", parent.ID, "sub-agent")
	require.NoError(t, err)

	childMsg, err := m.Create(ctx, child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "working..."}},
	})
	require.NoError(t, err)
	// Bump the child message's updated_at so it is unambiguously the freshest.
	childMsg.AppendContent(" still working")
	require.NoError(t, m.Update(ctx, childMsg))

	a := &app.App{Messages: m, Sessions: s}

	act := computeCallTreeActivity(ctx, a, parent.ID)
	require.NotZero(t, act.LatestUnix, "expected some activity in the tree")
	require.Equal(t, child.ID, act.DeepestSessionID, "freshest activity should be the child session")
	require.True(t, act.SubAgentActive, "child activity must flag SubAgentActive")
}

// TestComputeCallTreeActivity_NoChildren: a lone session with its own message
// and no sub-agents reports its own activity and does NOT flag SubAgentActive.
func TestComputeCallTreeActivity_NoChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(ctx, "solo")
	require.NoError(t, err)
	_, err = m.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	})
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	act := computeCallTreeActivity(ctx, a, sess.ID)
	require.NotZero(t, act.LatestUnix)
	require.Equal(t, sess.ID, act.DeepestSessionID)
	require.False(t, act.SubAgentActive)
}

// TestSubAgentActivityNote_OnlyWhenFresher: the shared note is empty when the
// baseline already covers the freshest activity (no in-flight sub-agent), and
// non-empty naming the child session when a sub-agent is the fresher edge.
func TestSubAgentActivityNote_OnlyWhenFresher(t *testing.T) {
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
		Parts: []message.ContentPart{message.TextContent{Text: "working"}},
	})
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	now := time.Now()

	// Baseline far in the past → child activity is fresher → note present.
	note := subAgentActivityNote(ctx, a, parent.ID, 0, now)
	require.NotEmpty(t, note)
	require.Contains(t, note, "sub-agent active")
	require.Contains(t, note, short(session.HashID(child.ID)))

	// Baseline far in the future → nothing is fresher → note empty.
	future := now.Add(time.Hour).Unix()
	require.Empty(t, subAgentActivityNote(ctx, a, parent.ID, future, now))
}
