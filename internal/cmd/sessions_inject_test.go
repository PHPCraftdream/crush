package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionsInject_Success(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "inject target")
	require.NoError(t, err)

	gotSess, msg, err := doInject(context.Background(), s, m, sess.ID, "hello from CLI", false)
	require.NoError(t, err)
	require.Equal(t, sess.ID, gotSess.ID)

	// Message created as a normal user message.
	msgs, err := m.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, msg.ID, msgs[0].ID)
	require.Equal(t, "hello from CLI", msgs[0].Content().Text)

	// pending_injects row created with the right message_id, not interrupt.
	injects, hasInterrupt, err := s.DrainPendingInjects(context.Background(), sess.ID)
	require.NoError(t, err)
	require.False(t, hasInterrupt)
	require.Len(t, injects, 1)
	require.Equal(t, msg.ID, injects[0].MessageID)
	require.Equal(t, sess.ID, injects[0].SessionID)
	require.False(t, injects[0].Interrupt)
	require.Equal(t, "hello from CLI", injects[0].Content)
}

func TestSessionsInject_NoMessageOrFile(t *testing.T) {
	t.Parallel()
	_, err := resolveInjectText("", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "exactly one")
}

func TestSessionsInject_BothMessageAndFile(t *testing.T) {
	t.Parallel()
	_, err := resolveInjectText("text", "some/file.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "exactly one")
}

func TestSessionsInject_FileRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "msg.md")
	require.NoError(t, os.WriteFile(path, []byte("from a file\n"), 0o644))

	text, err := resolveInjectText("", path)
	require.NoError(t, err)
	require.Equal(t, "from a file\n", text)
}

func TestSessionsInject_SessionNotFound(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	_, _, err := doInject(context.Background(), s, m, "does-not-exist", "hi", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "session not found")
}

func TestSessionsInject_InterruptFlag(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "interrupt target")
	require.NoError(t, err)

	_, msg, err := doInject(context.Background(), s, m, sess.ID, "stop now", true)
	require.NoError(t, err)

	// Interrupt rows are NOT drained by DrainPendingInjects; it only reports
	// their presence. Verify the flag round-tripped via a raw query.
	var interrupt int
	var messageID string
	row := conn.QueryRowContext(context.Background(),
		`SELECT interrupt, message_id FROM pending_injects WHERE session_id = ?`, sess.ID)
	require.NoError(t, row.Scan(&interrupt, &messageID))
	require.Equal(t, 1, interrupt)
	require.Equal(t, msg.ID, messageID)

	// DrainPendingInjects reports the pending interrupt but returns no rows.
	drained, hasInterrupt, err := s.DrainPendingInjects(context.Background(), sess.ID)
	require.NoError(t, err)
	require.True(t, hasInterrupt)
	require.Empty(t, drained)
}

func TestIsSessionLockAlive_FreshHeartbeatWithoutReadablePID(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	lockPath := filepath.Join(locksDir, "session-running.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(""), 0o644))

	require.True(t, isSessionLockAlive(dataDir, "running"))
}

// TestIsSessionLockAlive_StaleMtimeButLivePIDIsStillLive is the isSessionLockAlive-
// level regression test for task #229 (same root cause as task #228's
// session.InspectSessionLock fix, and task #222's sessions_watch.go
// combinedLockLiveness fix): a lock's heartbeat mtime is now gated on real
// RecordActivity() calls (task #213/#214), and the stream watchdog that
// supplies those calls while a tool is in flight only fires roughly every
// 30s (task #222) — larger than isSessionLockAlive's 20s threshold. That
// leaves a real window where a perfectly healthy, tool-busy session looks
// mtime-stale even though it's still genuinely running. A real second
// process holds the lock (spawnKillTestLockHolder, mirroring the pattern
// sessions_kill_test.go already established) so the PID-liveness fallback
// inherited from session.InspectSessionLock is exercised against a
// genuinely live OS process, not a same-process fake. We back-date the lock
// file's mtime past the threshold to simulate the heartbeat lagging behind
// a long tool call, then assert isSessionLockAlive still reports true via
// the PID fallback.
func TestIsSessionLockAlive_StaleMtimeButLivePIDIsStillLive(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}

	dataDir := t.TempDir()
	holder := spawnKillTestLockHolder(t, dataDir, "inject-stale-live")
	defer holder.stop()

	lockPath := filepath.Join(dataDir, "locks", "session-inject-stale-live.lock")
	staleTime := time.Now().Add(-(isSessionLockAliveThreshold + 5*time.Second))
	require.NoError(t, os.Chtimes(lockPath, staleTime, staleTime),
		"back-dating mtime to simulate a heartbeat lagging behind one long tool call on an otherwise-healthy session")

	require.True(t, session.IsProcessAlive(holder.pid), "helper process must still be alive for this test to be meaningful")

	require.True(t, isSessionLockAlive(dataDir, "inject-stale-live"),
		"a stale mtime must not override a genuinely live PID holder — this is the core #229 fix")
}

// TestIsSessionLockAlive_StaleMtimeAndDeadPIDIsNotLive is the conservative
// companion to the fix above: when mtime is stale AND the recorded PID is
// genuinely dead, isSessionLockAlive must still report false. The PID
// fallback must not regress the correct "actually dead" case into a false
// positive.
func TestIsSessionLockAlive_StaleMtimeAndDeadPIDIsNotLive(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	lockPath := filepath.Join(locksDir, "session-inject-stale-dead.lock")
	// PID 0x7FFFFFFE is virtually guaranteed not to be a running process
	// (same sentinel internal/session/lock_test.go's
	// TestInspectSessionLock_StaleMtimeAndDeadPIDIsNotLive uses).
	require.NoError(t, os.WriteFile(lockPath, []byte("2147483646\n"), 0o644))
	staleTime := time.Now().Add(-(isSessionLockAliveThreshold + 5*time.Second))
	require.NoError(t, os.Chtimes(lockPath, staleTime, staleTime))

	require.False(t, isSessionLockAlive(dataDir, "inject-stale-dead"))
}
