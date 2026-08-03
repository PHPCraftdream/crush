package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// writeLockFile creates a session lock file under tmpDir/locks/ holding
// the given PID (second line = optional timeout seconds), and returns the
// tmpDir so it can be passed to explainSessionStatus as its dataDir
// parameter — explainSessionStatus resolves the lock at
// <dataDir>/locks/session-<id>.lock (task #233 fix; previously this helper
// nested an extra ".crush" level to match the pre-fix <cwd>/.crush/locks
// layout).
func writeLockFile(t *testing.T, sessionID string, pid int) string {
	t.Helper()
	tmpDir := t.TempDir()
	locksDir := filepath.Join(tmpDir, "locks")
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

// TestExplainSessionStatus_Running_PIDUnreadableFreshHeartbeat reproduces
// the Windows scenario: tryLockFile's mandatory LockFileEx lock means the
// PID can't be read from another process while the holder is alive, so
// ReadLockPID returns 0 even though the session is genuinely running. A
// fresh heartbeat (recent mtime) must still classify this as "running", not
// "crashed" — see the Windows note on session.readLockFile.
func TestExplainSessionStatus_Running_PIDUnreadableFreshHeartbeat(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "live run, unreadable pid")
	require.NoError(t, err)

	// pid=0 simulates a lock file whose PID line couldn't be read.
	cwd := writeLockFile(t, sess.ID, 0)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: running")
	require.NotContains(t, out, "status: crashed")
}

// TestExplainSessionStatus_Crashed_PIDUnreadableStaleHeartbeat: pid=0 AND a
// stale heartbeat (old mtime) must still report "crashed" — the heartbeat
// fallback only rescues genuinely fresh locks, not abandoned ones.
func TestExplainSessionStatus_Crashed_PIDUnreadableStaleHeartbeat(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "dead run, unreadable pid")
	require.NoError(t, err)

	cwd := writeLockFile(t, sess.ID, 0)
	lockPath := filepath.Join(cwd, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	old := time.Now().Add(-30 * time.Second)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: crashed")
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

// TestExplainSessionStatus_PidReuseBeyondMaxFallbackAgeIsNotRunning is the
// regression test for task #250: explainSessionStatus (backing
// `crush sessions why`) was the FOURTH independent copy of the "trust a
// confirmed-alive PID unconditionally, with no bound on lock age" check that
// tasks #235/#241 had already bounded in the other three copies
// (InspectSessionLock, sessions_watch.go, sessions.go). A `crush run` killed
// with SIGKILL/taskkill /F leaves its PID in the lock file without
// releasing; hours later the OS can recycle that exact PID number for a
// completely unrelated, currently-running process. Before this fix,
// `sessions list` (already bounded) would correctly show crashed/done, but
// `crush sessions why` — the command whose ONLY job is to explain that very
// verdict — would say "running / lock held by live PID N", directly
// contradicting `sessions list` for the same session.
//
// A real second process (spawnKillTestLockHolder, the same cross-process
// harness sessions_kill_test.go uses) stands in for "the OS reused this
// exact PID number" — it is genuinely alive throughout, so this proves the
// AGE bound, not merely a dead-PID false negative. The lock file's mtime is
// back-dated past session.MaxPidFallbackAge to simulate a lock abandoned
// long enough ago that its recorded PID can no longer be trusted.
func TestExplainSessionStatus_PidReuseBeyondMaxFallbackAgeIsNotRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "why-status-pid-reuse")
	require.NoError(t, err)

	dataDir := t.TempDir()
	holder := spawnKillTestLockHolder(t, dataDir, sess.ID)
	defer holder.stop()
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	staleTime := time.Now().Add(-(session.MaxPidFallbackAge + 5*time.Second))
	require.NoError(t, os.Chtimes(lockPath, staleTime, staleTime),
		"back-dating mtime past MaxPidFallbackAge to simulate a lock abandoned long enough ago that its recorded PID can no longer be trusted, even though it currently resolves to a live process")

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))

	out := buf.String()
	require.NotContains(t, out, "status: running",
		"a lock older than MaxPidFallbackAge must not be reported running just because its recorded PID currently belongs to a live (but unrelated) process — this is the core #250 fix; sessions why must agree with sessions list")
	require.Contains(t, out, "status: crashed",
		"with no clean assistant finish, the bound-forced dead verdict must surface as crashed, matching what sessions list shows for the same session")
	require.Contains(t, out, "no longer trustworthy",
		"the bound-triggered reason must explain the recorded PID is untrusted due to age/PID reuse — not claim it is dead")
	require.NotContains(t, out, "is not alive",
		"the recorded PID is factually alive in this scenario (OS reuse, via spawnKillTestLockHolder) — claiming 'is not alive' would be the same factual lie task #250's verdict fix was meant to remove, relocated to the reason text (#256)")
}

// TestExplainSessionStatus_PidAliveWithinMaxFallbackAgeIsRunning is the
// non-regression companion: a live PID within MaxPidFallbackAge of the
// lock's mtime must still be reported "running", exactly like before this
// fix — the bound must only kick in once the lock is genuinely old. Uses a
// real second process (not the test's own PID) so it guards the same
// cross-process path the regression test does.
func TestExplainSessionStatus_PidAliveWithinMaxFallbackAgeIsRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "why-status-pid-fresh")
	require.NoError(t, err)

	dataDir := t.TempDir()
	holder := spawnKillTestLockHolder(t, dataDir, sess.ID)
	defer holder.stop()
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	justUnder := time.Now().Add(-(session.MaxPidFallbackAge - 2*time.Minute))
	require.NoError(t, os.Chtimes(lockPath, justUnder, justUnder))

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: running",
		"a live PID just under MaxPidFallbackAge must still be trusted as running")
	require.NotContains(t, out, "no longer trustworthy",
		"a fresh live PID within the bound must not trigger the PID-reuse/untrusted wording")
	require.NotContains(t, out, "is not alive",
		"a running session must not show any dead-case phrasing")
}
