package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
			worker_model_provider TEXT,
			worker_model_id TEXT,
			worker_model_reasoning_effort TEXT,
			reviewer_model_provider TEXT,
			reviewer_model_id TEXT,
			reviewer_model_reasoning_effort TEXT,
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0,
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0
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
			background_job_notice INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER,
			output_tokens INTEGER,
			reasoning_tokens INTEGER,
			cache_creation_tokens INTEGER,
			cache_read_tokens INTEGER,
			total_tokens INTEGER,
			cost_usd REAL,
			usage_provider TEXT,
			usage_model TEXT,
			cache_support TEXT,
			usage_estimated INTEGER
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

// TestRenderDelegationTranscript_MaxMessages verifies that only the most recent
// max_messages are included, with a pagination marker indicating how many were
// omitted.
func TestRenderDelegationTranscript_MaxMessages(t *testing.T) {
	t.Parallel()

	// Build 60 messages, each with distinct text.
	var msgs []message.Message
	for i := 0; i < 60; i++ {
		msgs = append(msgs, message.Message{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: fmt.Sprintf("message %d", i)},
			},
		})
	}

	out := renderDelegationTranscript("sess", msgs[50:], 60, ReadDelegationTranscriptParams{MaxMessages: 10, MaxBytes: 500_000})

	// Must contain the last 10 messages (50-59).
	require.Contains(t, out, "message 59")
	require.Contains(t, out, "message 50")
	// Must NOT contain earlier messages.
	require.NotContains(t, out, "message 49")
	require.NotContains(t, out, "message 0")
	// Must show pagination marker.
	require.Contains(t, out, "50 earlier omitted")
}

// TestRenderDelegationTranscript_MaxBytes verifies that the rendered output does
// not exceed the byte budget and includes a truncation marker when it would.
func TestRenderDelegationTranscript_MaxBytes(t *testing.T) {
	t.Parallel()

	// A single message with a very long text that exceeds the budget.
	longText := strings.Repeat("A", 10_000)
	msgs := []message.Message{
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: longText}}},
	}

	maxBytes := 500
	out := renderDelegationTranscript("sess", msgs, 1, ReadDelegationTranscriptParams{MaxMessages: 50, MaxBytes: maxBytes})

	require.Less(t, len(out), maxBytes+300,
		"output must be approximately within the byte budget (plus marker)")
	require.Contains(t, out, "truncated")
	require.Contains(t, out, "bytes dropped")
}

// TestRenderDelegationTranscript_Offset verifies offset pages backward from the
// end, skipping newer messages.
func TestRenderDelegationTranscript_Offset(t *testing.T) {
	t.Parallel()

	var msgs []message.Message
	for i := 0; i < 30; i++ {
		msgs = append(msgs, message.Message{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: fmt.Sprintf("msg %d", i)},
			},
		})
	}

	// offset=10, max_messages=10: should show messages 10-19 (0-indexed).
	out := renderDelegationTranscript("sess", msgs[10:20], 30, ReadDelegationTranscriptParams{MaxMessages: 10, Offset: 10, MaxBytes: 500_000})

	require.Contains(t, out, "msg 19")
	require.Contains(t, out, "msg 10")
	require.NotContains(t, out, "msg 20") // newer, skipped by offset
	require.NotContains(t, out, "msg 9")  // older, omitted
	require.Contains(t, out, "newer skipped")
}

// TestRenderDelegationTranscript_Defaults verifies that when no params are set,
// sensible defaults are used and no truncation marker appears for small inputs.
func TestRenderDelegationTranscript_Defaults(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "hello"}}},
	}

	out := renderDelegationTranscript("sess", msgs, 1, ReadDelegationTranscriptParams{})

	require.Contains(t, out, "hello")
	require.NotContains(t, out, "truncated")
	require.NotContains(t, out, "omitted")
}

// spyMessageService wraps a message.Service and records the arguments and
// result size of the most recent ListPaginatedSnapshot call, so a test can
// prove the tool read only a bounded window from the DB instead of the whole
// history. read_delegation_transcript.go reads via ListPaginatedSnapshot (not
// separate Count + ListPaginated calls) specifically to keep the "N earlier
// omitted" total consistent with the returned window under concurrent
// inserts — see ListPaginatedSnapshot's doc comment on message.Service.
type spyMessageService struct {
	message.Service
	listPaginatedLimit  int
	listPaginatedOffset int
	listPaginatedLen    int
	listPaginatedCalled bool
}

func (s *spyMessageService) ListPaginatedSnapshot(ctx context.Context, sessionID string, limit, offset int) ([]message.Message, int64, error) {
	msgs, total, err := s.Service.ListPaginatedSnapshot(ctx, sessionID, limit, offset)
	s.listPaginatedLimit = limit
	s.listPaginatedOffset = offset
	s.listPaginatedLen = len(msgs)
	s.listPaginatedCalled = true
	return msgs, total, err
}

// TestReadDelegationTranscript_PaginationReadsBoundedWindow proves the tool
// pages at the SQL layer: a session with 1000 messages read with
// max_messages=10 must request (and receive) only ~10 rows from
// ListPaginated, not decode all 1000, while still reporting the true total in
// its "N earlier omitted" marker.
func TestReadDelegationTranscript_PaginationReadsBoundedWindow(t *testing.T) {
	t.Parallel()
	s, m := newTranscriptTestDB(t)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "m$$c", parent.ID, "worker")
	require.NoError(t, err)

	// Seed 1000 messages so the full history is far larger than the window.
	for i := 0; i < 1000; i++ {
		_, err = m.Create(context.Background(), child.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("line %d", i)}},
		})
		require.NoError(t, err)
	}

	spy := &spyMessageService{Service: m}
	tool := NewReadDelegationTranscriptTool(s, spy)
	input, err := json.Marshal(ReadDelegationTranscriptParams{SessionID: child.ID, MaxMessages: 10, MaxBytes: 1_500_000})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, parent.ID)
	resp, runErr := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: ReadDelegationTranscriptToolName, Input: string(input)})

	require.NoError(t, runErr)
	require.False(t, resp.IsError, "reading a legitimate child must succeed: %s", resp.Content)

	// The DB query must have asked for only the window size, and received at
	// most that many rows — not the full 1000-message history.
	require.True(t, spy.listPaginatedCalled, "tool must read via ListPaginatedSnapshot, not List")
	require.Equal(t, 10, spy.listPaginatedLimit, "ListPaginatedSnapshot must request only the window size")
	require.LessOrEqual(t, spy.listPaginatedLen, 10, "ListPaginatedSnapshot must return at most the window size")
	// The marker must still reflect the true total of 1000.
	require.Contains(t, resp.Content, "990 earlier omitted")
}

// TestRenderDelegationTranscript_HardMaxBytesClamp proves an absurdly large
// max_bytes (10^9) is clamped to hardMaxTranscriptBytes rather than passing
// through: a payload that would otherwise fit entirely under 10^9 must still
// be truncated near the hard ceiling.
func TestRenderDelegationTranscript_HardMaxBytesClamp(t *testing.T) {
	t.Parallel()

	// 5 MiB of text would fit comfortably under a 10^9 budget if unclamped.
	huge := strings.Repeat("B", 5_000_000)
	msgs := []message.Message{
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: huge}}},
	}

	out := renderDelegationTranscript("sess", msgs, 1, ReadDelegationTranscriptParams{MaxBytes: 1_000_000_000})

	require.Less(t, len(out), hardMaxTranscriptBytes+1024,
		"absurd max_bytes must be clamped to hardMaxTranscriptBytes, not pass through")
	require.Contains(t, out, "truncated")
}

// TestRenderDelegationTranscript_HugeMessageBounded proves a single content
// part carrying several MiB does not blow past the byte budget: the output
// stays within max_bytes (plus a small marker margin), confirming the budget
// is checked per-part at write time rather than after assembling the whole
// message.
func TestRenderDelegationTranscript_HugeMessageBounded(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("Z", 3_000_000) // 3 MiB in a single content part.
	msgs := []message.Message{
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: huge}}},
	}

	out := renderDelegationTranscript("sess", msgs, 1, ReadDelegationTranscriptParams{MaxBytes: 50_000})

	require.Less(t, len(out), 50_000+1024,
		"a huge single-part message must not blow past the byte budget")
	require.Contains(t, out, "truncated")
	require.Contains(t, out, "bytes dropped")
}
