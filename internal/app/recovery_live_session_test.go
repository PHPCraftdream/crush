package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRecoveryTestAppWithDataDir is newRecoveryTestApp plus a data directory
// for the cross-process liveness probe (task #287). The zero orphan-age
// threshold is deliberate: it removes the 30s age heuristic from the picture
// entirely, so these tests prove the LOCK probe is what protects a live
// session — not the age filter accidentally covering for it.
func newRecoveryTestAppWithDataDir(t *testing.T, dataDir string) *App {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	zero := time.Duration(0)
	return &App{
		Sessions:          session.NewService(q, conn),
		Messages:          message.NewService(q),
		recoveryOrphanAge: &zero,
		recoveryDataDir:   &dataDir,
	}
}

// newOrphanAssistant creates the shape a process death leaves behind: an
// assistant message with a tool call and NO finish part.
func newOrphanAssistant(t *testing.T, app *App, sessionID string) message.Message {
	t.Helper()
	orphan, err := app.Messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "working on it..."},
			message.ToolCall{ID: "call_1", Name: "bash", Input: `{"command":"echo hi"}`, Finished: true},
		},
	})
	require.NoError(t, err)
	require.False(t, orphan.IsFinished(), "precondition: orphan must NOT have a finish part")
	return orphan
}

// TestRecoverInterruptedTurns_LiveLockHolder_LeavesSessionAlone is the
// regression test for release-blocker #287.
//
// recoverInterruptedTurns runs at the start of EVERY crush process and
// iterates EVERY session in the data directory — not just the ones this
// process owns. Before the fix its only guard was a 30-second age threshold,
// so any turn running longer than that in process A was stamped
// "Process restarted" by process B merely starting up. Because
// message.Update rewrites the entire Parts blob from the snapshot the sweep
// read, that stamp also clobbers whatever the live owner streamed in
// between. A sub-agent delegation is bounded at 45 minutes, so in practice
// essentially every delegation was exposed; an observed 38-minute run in a
// sibling repo was killed exactly this way, producing zero file edits.
//
// Here the session's lock is genuinely held (by this test process, which is
// unambiguously alive), and the orphan-age threshold is ZERO — so nothing
// but a real liveness probe can save the message. If the probe regresses,
// the assistant gets a finish part and this test fails.
func TestRecoverInterruptedTurns_LiveLockHolder_LeavesSessionAlone(t *testing.T) {
	dataDir := t.TempDir()
	app := newRecoveryTestAppWithDataDir(t, dataDir)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "live-in-another-process")
	require.NoError(t, err)
	orphan := newOrphanAssistant(t, app, sess.ID)

	// Hold the session lock for real, exactly as a live owner would.
	lk, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err, "precondition: the test must be able to hold the session lock")
	t.Cleanup(func() { _ = lk.Release() })

	require.True(t, session.InspectSessionLock(dataDir, sess.ID, session.LockStaleDuration).Live,
		"precondition: the lock we just acquired must read as live")

	app.recoverInterruptedTurns(ctx)

	got, err := app.Messages.Get(ctx, orphan.ID)
	require.NoError(t, err)
	assert.False(t, got.IsFinished(),
		"recovery must NOT stamp a session whose lock is held by a live process — "+
			"doing so marks an in-flight turn as errored and clobbers its streamed content (#287)")
	assert.Nil(t, got.FinishPart(),
		"no finish part may be added to a live session's in-flight assistant message")

	// The content the live owner had written must survive untouched.
	assert.Equal(t, "working on it...", got.Content().Text,
		"the sweep must not rewrite a live session's message content")
}

// TestRecoverInterruptedTurns_NoLiveHolder_StillRecovers is the companion
// non-regression test: the fix must not disable recovery for its actual
// purpose. A genuinely dead holder — lock acquired then released, exactly
// what a cleanly-exited or crashed-and-reaped process leaves — must still be
// recovered.
func TestRecoverInterruptedTurns_NoLiveHolder_StillRecovers(t *testing.T) {
	dataDir := t.TempDir()
	app := newRecoveryTestAppWithDataDir(t, dataDir)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "genuinely-orphaned")
	require.NoError(t, err)
	orphan := newOrphanAssistant(t, app, sess.ID)

	// Acquire, release, then back-date the lock file: the real-world
	// "previous process died a while ago" shape. Release clears the holder
	// PID but leaves the FILE in place and touches its mtime, so a
	// just-released lock deliberately still reads Live for
	// LockStaleDuration (20s) — a benign window, since recovery is a
	// startup safety net rather than a time-critical operation. Back-dating
	// past that window is what makes this session unambiguously ownerless.
	lk, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err)
	require.NoError(t, lk.Release())

	lockPath := filepath.Join(dataDir, "locks", "session-"+sess.ID+".lock")
	stale := time.Now().Add(-(session.LockStaleDuration + time.Minute))
	require.NoError(t, os.Chtimes(lockPath, stale, stale))
	require.False(t, session.InspectSessionLock(dataDir, sess.ID, session.LockStaleDuration).Live,
		"precondition: a released, back-dated lock must read as not-live (Release clears the holder PID, so no PID fallback applies)")

	app.recoverInterruptedTurns(ctx)

	got, err := app.Messages.Get(ctx, orphan.ID)
	require.NoError(t, err)
	assert.True(t, got.IsFinished(),
		"recovery must still rescue a genuinely orphaned turn — the #287 fix must not disable the safety net itself")
	assert.Equal(t, message.FinishReasonError, got.FinishReason())
	fp := got.FinishPart()
	require.NotNil(t, fp)
	assert.Equal(t, "Process restarted", fp.Message)
}

// TestRecoverInterruptedTurns_NoLockFileAtAll_StillRecovers covers the
// session that never had a lock file (or whose lock was already reaped):
// InspectSessionLock reports Exists:false / Live:false, and recovery must
// proceed exactly as it did before the fix.
func TestRecoverInterruptedTurns_NoLockFileAtAll_StillRecovers(t *testing.T) {
	dataDir := t.TempDir()
	app := newRecoveryTestAppWithDataDir(t, dataDir)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "no-lock-file")
	require.NoError(t, err)
	orphan := newOrphanAssistant(t, app, sess.ID)

	require.False(t, session.InspectSessionLock(dataDir, sess.ID, session.LockStaleDuration).Exists,
		"precondition: no lock file must exist for this session")

	app.recoverInterruptedTurns(ctx)

	got, err := app.Messages.Get(ctx, orphan.ID)
	require.NoError(t, err)
	assert.True(t, got.IsFinished(),
		"a session with no lock file at all has no live owner — recovery must proceed")
	assert.Equal(t, message.FinishReasonError, got.FinishReason())
}

// TestRecoverInterruptedTurns_LiveAndDeadSessions_OnlyDeadRecovered proves
// the probe discriminates per-session rather than disabling the whole sweep:
// in one directory, a live-locked session is skipped while a dead one in the
// same pass is still recovered.
func TestRecoverInterruptedTurns_LiveAndDeadSessions_OnlyDeadRecovered(t *testing.T) {
	dataDir := t.TempDir()
	app := newRecoveryTestAppWithDataDir(t, dataDir)
	ctx := t.Context()

	liveSess, err := app.Sessions.Create(ctx, "live")
	require.NoError(t, err)
	liveOrphan := newOrphanAssistant(t, app, liveSess.ID)

	deadSess, err := app.Sessions.Create(ctx, "dead")
	require.NoError(t, err)
	deadOrphan := newOrphanAssistant(t, app, deadSess.ID)

	lk, err := session.TryAcquireSessionLock(dataDir, liveSess.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() })

	app.recoverInterruptedTurns(ctx)

	gotLive, err := app.Messages.Get(ctx, liveOrphan.ID)
	require.NoError(t, err)
	assert.False(t, gotLive.IsFinished(), "the live-locked session must be skipped")

	gotDead, err := app.Messages.Get(ctx, deadOrphan.ID)
	require.NoError(t, err)
	assert.True(t, gotDead.IsFinished(), "the session with no live holder must still be recovered in the same pass")
}
