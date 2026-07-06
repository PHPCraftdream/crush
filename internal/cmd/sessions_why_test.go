package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// writeLockFile creates a session lock file under tmpDir/.crush/locks/ holding
// the given PID (second line = optional timeout seconds), and returns the tmpDir
// so it can be passed to explainSessionStatus as cwd.
func writeLockFile(t *testing.T, sessionID string, pid int) string {
	t.Helper()
	tmpDir := t.TempDir()
	locksDir := filepath.Join(tmpDir, ".crush", "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(strconv.Itoa(pid)+"\n"), 0o644))
	return tmpDir
}

// TestExplainSessionStatus_Done_EndTurn: no lock file → "at rest", but the
// last assistant message finished with end_turn, so the output should say
// the session is idle and mention end_turn.
func TestExplainSessionStatus_AtRest_CleanFinish(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "clean idle")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "do it"}},
	})
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "done"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	// t.TempDir() with no locks dir created → no lock file → "at rest".
	require.NoError(t, explainSessionStatus(context.Background(), a, t.TempDir(), sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: at rest")
	require.Contains(t, out, "end_turn")
}

// TestExplainSessionStatus_Crashed_NoCleanFinish: lock file exists, holder PID
// is dead, and the last assistant message did NOT finish cleanly → verdict is
// "crashed" and the reason mentions dying mid-turn.
func TestExplainSessionStatus_Crashed_NoCleanFinish(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "mid-turn crash")
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial"}},
	})
	require.NoError(t, err)
	// Canceled, not end_turn — no clean finish.
	assistant.AddFinish(message.FinishReasonCanceled, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	// PID 999999 is guaranteed not to be a live process on any platform.
	cwd := writeLockFile(t, sess.ID, 999999)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: crashed")
	require.Contains(t, out, "died mid-turn")
	require.Contains(t, out, "canceled")
}

// TestExplainSessionStatus_StaleLockCleanFinish: lock file exists, holder
// PID is dead, BUT the last assistant message finished with end_turn. The raw
// lock signal says "crashed" but the message store contradicts it → the FIRST
// LINE verdict must be "done (stale lock)" — matching what `sessions list`
// shows after reclassifyCrashedAsDone, so orchestrators parsing the first line
// get the same verdict from both commands. The reason must mention the clean
// finish + stale lock, and the NOTE must still say "Treat as done".
func TestExplainSessionStatus_StaleLockCleanFinish(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "stale lock clean exit")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "run"}},
	})
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "finished ok"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	cwd := writeLockFile(t, sess.ID, 999999)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, cwd, sess.ID, &buf))

	out := buf.String()
	// First-line verdict must say done (matching sessions list), NOT crashed.
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: done (stale lock)", firstLine,
		"first line must say done (stale lock), not crashed — must match sessions list verdict")
	// Must NOT say crashed anywhere in the output now.
	require.NotContains(t, out, "status: crashed")
	// The explanation must still call out the clean finish + stale lock.
	require.Contains(t, out, "finished cleanly (end_turn)")
	require.Contains(t, out, "stale lock")
	require.Contains(t, out, "Treat as done")
}

// TestExplainSessionStatus_AtRest_NoAssistantMessage: no lock file and no
// assistant message at all → "at rest" with the "no assistant message" note.
func TestExplainSessionStatus_AtRest_NoAssistantMessage(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "empty")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, t.TempDir(), sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: at rest")
	require.Contains(t, out, "no assistant message recorded yet")
}

// TestExplainSessionStatus_Running: lock file exists and holder PID is alive
// (we use our own PID) → verdict is "running" and the heartbeat age is shown.
func TestExplainSessionStatus_Running(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "live run")
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "working"}},
	})
	require.NoError(t, err)
	// Tool use finish — turn still in progress from the user's perspective,
	// but the message has a finish part we can report.
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	cwd := writeLockFile(t, sess.ID, os.Getpid())

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: running")
	require.Contains(t, out, "heartbeat")
	require.Contains(t, out, "tool_use")
}

// TestExplainSessionStatus_ErrorFinishSurfacesErrorText: when the last
// assistant message finished with FinishReasonError and stored error text,
// the output must include that error text so the operator sees the cause.
func TestExplainSessionStatus_ErrorFinishSurfacesErrorText(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "errored")
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "oops"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonError, "upstream 502: bad gateway", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	cwd := writeLockFile(t, sess.ID, 999999)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: crashed")
	require.Contains(t, out, "died mid-turn")
	require.Contains(t, out, "error")
	require.Contains(t, out, "upstream 502: bad gateway")
}
