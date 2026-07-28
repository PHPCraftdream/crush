package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func newTranscriptTestDB(t *testing.T) (session.Service, message.Service) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })

	_, err = sqlDB.ExecContext(context.Background(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			summary_message_id TEXT,
			todos TEXT,
			deleted_todos TEXT NOT NULL DEFAULT '[]',
			large_model_provider TEXT,
			large_model_id TEXT,
			large_model_reasoning_effort TEXT DEFAULT 'medium',
			small_model_provider TEXT,
			small_model_id TEXT,
			small_model_reasoning_effort TEXT DEFAULT 'medium',
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			provider TEXT,
			reasoning_effort TEXT DEFAULT 'medium',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER,
			is_summary_message INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			hidden INTEGER NOT NULL DEFAULT 0,
			auto_resumed INTEGER NOT NULL DEFAULT 0,
			background_job_notice INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX idx_messages_session_id ON messages(session_id);
	`)
	require.NoError(t, err)

	q := db.New(sqlDB)
	return session.NewService(q, sqlDB), message.NewService(q)
}

func runTranscriptTool(t *testing.T, s session.Service, m message.Service, callerID, targetID string) fantasy.ToolResponse {
	t.Helper()
	tool := NewReadDelegationTranscriptTool(s, m)
	require.Equal(t, ReadDelegationTranscriptToolName, tool.Info().Name)
	input, err := json.Marshal(ReadDelegationTranscriptParams{SessionID: targetID})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, callerID)
	resp, runErr := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: ReadDelegationTranscriptToolName, Input: string(input)})
	require.NoError(t, runErr)
	return resp
}

// TestReadDelegationTranscript_ReadsChild confirms the orchestrator can read a
// direct sub-agent child's transcript.
func TestReadDelegationTranscript_ReadsChild(t *testing.T) {
	t.Parallel()
	s, m := newTranscriptTestDB(t)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "msg$$call", parent.ID, "worker")
	require.NoError(t, err)
	_, err = m.Create(context.Background(), child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "child did the work"}},
	})
	require.NoError(t, err)

	resp := runTranscriptTool(t, s, m, parent.ID, child.ID)
	require.False(t, resp.IsError, "reading a legitimate child must succeed: %s", resp.Content)
	require.Contains(t, resp.Content, "child did the work")
	require.Contains(t, resp.Content, child.ID)
}

// TestReadDelegationTranscript_RefusesUnrelated confirms an unrelated (non-
// descendant) session id is refused — the security check mirroring runSubAgent.
func TestReadDelegationTranscript_RefusesUnrelated(t *testing.T) {
	t.Parallel()
	s, m := newTranscriptTestDB(t)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)
	other, err := s.Create(context.Background(), "someone else")
	require.NoError(t, err)
	_, err = m.Create(context.Background(), other.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "secret"}},
	})
	require.NoError(t, err)

	resp := runTranscriptTool(t, s, m, parent.ID, other.ID)
	require.True(t, resp.IsError, "an unrelated session must be refused")
	require.NotContains(t, resp.Content, "secret")
}

// TestReadDelegationTranscript_RefusesSelf confirms passing the caller's own
// session id is rejected (not a delegation).
func TestReadDelegationTranscript_RefusesSelf(t *testing.T) {
	t.Parallel()
	s, m := newTranscriptTestDB(t)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)

	resp := runTranscriptTool(t, s, m, parent.ID, parent.ID)
	require.True(t, resp.IsError, "reading own session must be refused")
}

// flakySubSessionsService wraps a session.Service and forces ListSubSessions
// to fail once the walk reaches failAt, simulating a transient DB error on an
// intermediate node of the descendant tree.
type flakySubSessionsService struct {
	session.Service
	failAt string
}

func (f *flakySubSessionsService) ListSubSessions(ctx context.Context, parentSessionID string) ([]session.Session, error) {
	if parentSessionID == f.failAt {
		return nil, errors.New("simulated transient DB error")
	}
	return f.Service.ListSubSessions(ctx, parentSessionID)
}

// TestReadDelegationTranscript_TreeWalkErrorIsSurfaced confirms that a DB
// error partway through the ownership walk (isDescendantSession) is surfaced
// to the caller as a real Go error rather than being silently swallowed and
// treated as "not a descendant". Before the fix, isDescendantSession would
// `continue` past the failing node, walk to the end of the (now-truncated)
// tree, and return a plain `false` — indistinguishable from a genuine
// ownership refusal — even though the target session (a real grandchild) was
// only unreachable because we failed to check.
func TestReadDelegationTranscript_TreeWalkErrorIsSurfaced(t *testing.T) {
	t.Parallel()
	s, m := newTranscriptTestDB(t)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "m1$$c1", parent.ID, "worker")
	require.NoError(t, err)
	grandchild, err := s.CreateTaskSession(context.Background(), "m2$$c2", child.ID, "sub-worker")
	require.NoError(t, err)
	_, err = m.Create(context.Background(), grandchild.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "deep work"}},
	})
	require.NoError(t, err)

	// Fail ListSubSessions specifically on the intermediate "child" node, the
	// one whose children include the real target (grandchild). Without the
	// fix this would make the whole branch invisible and the walk would
	// finish having never found grandchild, returning a false "not a
	// descendant" refusal instead of surfacing the failure.
	flaky := &flakySubSessionsService{Service: s, failAt: child.ID}

	tool := NewReadDelegationTranscriptTool(flaky, m)
	input, err := json.Marshal(ReadDelegationTranscriptParams{SessionID: grandchild.ID})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, parent.ID)
	resp, runErr := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: ReadDelegationTranscriptToolName, Input: string(input)})

	require.Error(t, runErr, "a mid-walk DB error must surface as a real error, not a silent false/refusal")
	require.NotContains(t, runErr.Error(), "is not a sub-agent delegation",
		"the error must be distinguishable from a genuine ownership refusal")
	require.Contains(t, runErr.Error(), "failed to verify session ownership")
	// The tool must not have returned a (nil-error) refusal response either.
	require.Empty(t, resp.Content)
}

// TestReadDelegationTranscript_ReadsGrandchild confirms nested delegations
// (a child of a child) are reachable through the descendant walk.
func TestReadDelegationTranscript_ReadsGrandchild(t *testing.T) {
	t.Parallel()
	s, m := newTranscriptTestDB(t)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "m1$$c1", parent.ID, "worker")
	require.NoError(t, err)
	grandchild, err := s.CreateTaskSession(context.Background(), "m2$$c2", child.ID, "sub-worker")
	require.NoError(t, err)
	_, err = m.Create(context.Background(), grandchild.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "deep work"}},
	})
	require.NoError(t, err)

	resp := runTranscriptTool(t, s, m, parent.ID, grandchild.ID)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "deep work")
}
