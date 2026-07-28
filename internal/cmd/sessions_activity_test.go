package cmd

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

// flakyCallTreeSessionService wraps a session.Service and forces
// GetCallTreeActivity / GetCallTreeActivityBatch to fail, simulating a
// transient DB error on the aggregate call-tree query. Since the call-tree
// signal is now computed by ONE SQL query instead of a per-node BFS, a
// failure can no longer be scoped to "one node in the tree" — it is either
// the whole query succeeding or the whole query failing.
type flakyCallTreeSessionService struct {
	session.Service
	fail bool
}

func (f *flakyCallTreeSessionService) GetCallTreeActivity(ctx context.Context, rootID string) (session.CallTreeActivity, bool, error) {
	if f.fail {
		return session.CallTreeActivity{}, false, errors.New("simulated transient DB error")
	}
	return f.Service.GetCallTreeActivity(ctx, rootID)
}

func (f *flakyCallTreeSessionService) GetCallTreeActivityBatch(ctx context.Context, rootIDs []string) (map[string]session.CallTreeActivity, error) {
	if f.fail {
		return nil, errors.New("simulated transient DB error")
	}
	return f.Service.GetCallTreeActivityBatch(ctx, rootIDs)
}

// slogRecordCapture is a minimal slog.Handler that records every log record
// it receives, so a test can assert a specific message was actually logged
// rather than merely that the code path didn't panic.
type slogRecordCapture struct {
	records []slog.Record
}

func (c *slogRecordCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *slogRecordCapture) Handle(_ context.Context, r slog.Record) error {
	c.records = append(c.records, r)
	return nil
}
func (c *slogRecordCapture) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *slogRecordCapture) WithGroup(_ string) slog.Handler      { return c }

func (c *slogRecordCapture) hasMessageContaining(substr string) bool {
	for _, r := range c.records {
		if strings.Contains(r.Message, substr) {
			return true
		}
	}
	return false
}

// TestComputeCallTreeActivity_QueryErrorIsLogged confirms that when the
// aggregate call-tree query fails, computeCallTreeActivity (a) returns the
// zero-value callTreeActivity — this is a best-effort diagnostic signal, so
// a query failure degrades to "no activity found" rather than propagating
// as an error to any of the six call-tree consumers — and (b) actually logs
// the swallowed error at Debug level so it is visible under verbose/debug
// logging instead of silently vanishing.
func TestComputeCallTreeActivity_QueryErrorIsLogged(t *testing.T) {
	ctx := context.Background()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(ctx, "parent")
	require.NoError(t, err)
	_, err = m.Create(ctx, parent.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	flakyS := &flakyCallTreeSessionService{Service: s, fail: true}
	a := &app.App{Messages: m, Sessions: flakyS}

	capture := &slogRecordCapture{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(capture))
	defer slog.SetDefault(origLogger)

	act := computeCallTreeActivity(ctx, a, parent.ID)

	// A whole-query failure degrades to the zero-value signal.
	require.Zero(t, act.LatestUnix)
	require.Empty(t, act.DeepestSessionID)
	require.False(t, act.SubAgentActive)

	require.True(t, capture.hasMessageContaining("query failed"),
		"the swallowed GetCallTreeActivity error must be logged at Debug level, not silently dropped")
}

// TestComputeCallTreeActivityBatch_MatchesPerRootCalls builds two independent
// root sessions with their own sub-agent children and asserts the batch
// helper returns, for EACH root, exactly what a per-root
// computeCallTreeActivity call would have returned — i.e. batching does not
// let one root's tree leak into another's result.
func TestComputeCallTreeActivityBatch_MatchesPerRootCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	rootA, err := s.Create(ctx, "root-a")
	require.NoError(t, err)
	_, err = m.Create(ctx, rootA.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "a"}},
	})
	require.NoError(t, err)
	childA, err := s.CreateTaskSession(ctx, "msg$$callA", rootA.ID, "sub-agent-a")
	require.NoError(t, err)
	childAMsg, err := m.Create(ctx, childA.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "working a"}},
	})
	require.NoError(t, err)
	childAMsg.AppendContent(" more a")
	require.NoError(t, m.Update(ctx, childAMsg))

	rootB, err := s.Create(ctx, "root-b")
	require.NoError(t, err)
	_, err = m.Create(ctx, rootB.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "b, no children"}},
	})
	require.NoError(t, err)

	// A root with no messages at all — must be absent from the batch map.
	rootC, err := s.Create(ctx, "root-c")
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}

	batch := computeCallTreeActivityBatch(ctx, a, []string{rootA.ID, rootB.ID, rootC.ID})

	perA := computeCallTreeActivity(ctx, a, rootA.ID)
	perB := computeCallTreeActivity(ctx, a, rootB.ID)

	require.Equal(t, perA, batch[rootA.ID], "root A's batched result must match its per-root result")
	require.True(t, batch[rootA.ID].SubAgentActive, "root A's freshest activity is on its child")
	require.Equal(t, childA.ID, batch[rootA.ID].DeepestSessionID)

	require.Equal(t, perB, batch[rootB.ID], "root B's batched result must match its per-root result")
	require.False(t, batch[rootB.ID].SubAgentActive, "root B has no children")
	require.Equal(t, rootB.ID, batch[rootB.ID].DeepestSessionID)

	_, ok := batch[rootC.ID]
	require.False(t, ok, "a root with no activity anywhere in its tree must be absent from the batch map")
}
