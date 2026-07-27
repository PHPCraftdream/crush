package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestPrintSubAgentTranscripts_Text verifies that a parent session's direct
// sub-agent child sessions are rendered as demarcated, indented blocks — and
// that a session with NO children produces no output at all (the default,
// opt-out state the flag guards).
func TestPrintSubAgentTranscripts_Text(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)

	// A parent with no children renders nothing.
	var empty bytes.Buffer
	a := &app.App{Messages: m, Sessions: s}
	printSubAgentTranscripts(context.Background(), &empty, a, parent.ID, "text", time.Now())
	require.Empty(t, empty.String(), "no children -> no output")

	// Add a sub-agent child session with a couple of messages.
	child, err := s.CreateTaskSession(context.Background(), "msg-1$$call-1", parent.ID, "worker")
	require.NoError(t, err)
	_, err = m.Create(context.Background(), child.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "delegated task"}},
	})
	require.NoError(t, err)
	_, err = m.Create(context.Background(), child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "sub-agent reply"}},
	})
	require.NoError(t, err)

	var out bytes.Buffer
	printSubAgentTranscripts(context.Background(), &out, a, parent.ID, "text", time.Now())
	got := out.String()

	require.Contains(t, got, "sub-agent delegation")
	require.Contains(t, got, "worker")
	require.Contains(t, got, "delegated task")
	require.Contains(t, got, "sub-agent reply")
	require.Contains(t, got, "end sub-agent delegation")
	// Every child content line must be indented with the block prefix so it
	// can't be confused with the parent's own stream.
	require.Contains(t, got, "│ ")
}

// TestPrintSubAgentTranscripts_NDJSON verifies the machine-readable path tags
// each child block with a sub_agent_session marker line.
func TestPrintSubAgentTranscripts_NDJSON(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(context.Background(), "parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "msg-2$$call-2", parent.ID, "worker")
	require.NoError(t, err)
	_, err = m.Create(context.Background(), child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	})
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	var out bytes.Buffer
	printSubAgentTranscripts(context.Background(), &out, a, parent.ID, "ndjson", time.Now())
	got := out.String()
	require.Contains(t, got, `"sub_agent_session":"`+child.ID+`"`)
	require.Contains(t, got, `"role":"assistant"`)
}

// TestIndentWriter prefixes every line, including one produced across
// multiple Write calls, and does not emit a stray prefix after a final
// trailing newline.
func TestIndentWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	iw := &indentWriter{w: &buf, prefix: []byte("> ")}
	// Split a two-line payload across writes to exercise the mid-line state.
	_, _ = iw.Write([]byte("first li"))
	_, _ = iw.Write([]byte("ne\nsecond line\n"))

	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	require.Equal(t, []string{"> first line", "> second line"}, lines)
	// No dangling prefix after the trailing newline.
	require.False(t, strings.HasSuffix(got, "> "))
}
