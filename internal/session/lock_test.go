package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTryAcquireSessionLock_HappyPath(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	require.NotNil(t, lk)

	assert.Equal(t, os.Getpid(), lk.HolderPID)
	// Lock file must exist on disk.
	_, statErr := os.Stat(lk.Path)
	assert.NoError(t, statErr)

	// Verify PID was stamped into the file — only readable after we
	// release (Windows file-sharing semantics: a concurrent reader on
	// the same path while we hold an exclusive lock may see an empty
	// view depending on OS-level cache). In production this is fine
	// because the contending crush process gets the busy-error path,
	// reads the file AFTER our handle is closed, and sees the PID.
	require.NoError(t, lk.Release())
	bts, err := os.ReadFile(lk.Path)
	require.NoError(t, err)
	assert.Contains(t, string(bts), "\n",
		"after release, lock file must contain PID line so the next holder can name us in its busy error")
}

func TestTryAcquireSessionLock_ReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	lk1, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	require.NoError(t, lk1.Release())

	// After Release, a fresh acquire of the same session id must succeed.
	lk2, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	require.NoError(t, lk2.Release())
}

func TestTryAcquireSessionLock_DifferentSessions(t *testing.T) {
	dir := t.TempDir()
	lkA, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	defer lkA.Release()
	lkB, err := TryAcquireSessionLock(dir, "audit-B")
	require.NoError(t, err)
	defer lkB.Release()

	// Different session ids → different lock files → both succeed
	// concurrently. This is the common workflow case (5 parallel audits).
	assert.NotEqual(t, lkA.Path, lkB.Path)
}

func TestTryAcquireSessionLock_ReleaseNilSafe(t *testing.T) {
	var lk *SessionLock
	require.NoError(t, lk.Release())
}

func TestTryAcquireSessionLock_ReleaseTwice(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "s")
	require.NoError(t, err)
	require.NoError(t, lk.Release())
	// Second release is harmless.
	require.NoError(t, lk.Release())
}

func TestTryAcquireSessionLock_ConcurrentRelease(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "s")
	require.NoError(t, err)

	// 10 goroutines racing to Release — must not panic (double-close).
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = lk.Release()
		}()
	}
	wg.Wait()
}

func TestTryAcquireSessionLock_BadInputs(t *testing.T) {
	_, err := TryAcquireSessionLock("", "s")
	require.Error(t, err)
	_, err = TryAcquireSessionLock(t.TempDir(), "")
	require.Error(t, err)
}

func TestSanitiseSessionID(t *testing.T) {
	// Real session ids in this codebase: uuids, caller-chosen tags
	// like "audit-A", "pr-42". Belt-and-suspenders against future schemes
	// that include path separators or shell-special chars.
	cases := map[string]string{
		"audit-A":                              "audit-A",
		"abc/def":                              "abc_def",
		"abc\\def":                             "abc_def",
		"weird:id:with:colons":                 "weird_id_with_colons",
		"a*b?c\"d<e>f|g h":                     "a_b_c_d_e_f_g_h",
		"550e8400-e29b-41d4-a716-446655440000": "550e8400-e29b-41d4-a716-446655440000",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitiseSessionID(in), "input: %q", in)
	}
}

func TestSessionLockBusyError_Format(t *testing.T) {
	e := &SessionLockBusyError{Path: "/tmp/x.lock", HolderPID: 1234}
	assert.Contains(t, e.Error(), "1234")
	assert.Contains(t, e.Error(), "/tmp/x.lock")

	e2 := &SessionLockBusyError{Path: "/tmp/x.lock"}
	assert.Contains(t, e2.Error(), "another crush process")

	// Must be detectable via errors.As — caller-visible contract.
	var target *SessionLockBusyError
	wrapped := wrap(e)
	assert.True(t, errors.As(wrapped, &target))
	assert.Equal(t, 1234, target.HolderPID)
}

// wrap helper: simulates a caller fmt.Errorf'ing around the busy error.
func wrap(err error) error {
	type wrapper struct{ inner error }
	w := &wrapper{inner: err}
	_ = w
	return &errWithCause{msg: "outer: " + err.Error(), cause: err}
}

type errWithCause struct {
	msg   string
	cause error
}

func (e *errWithCause) Error() string { return e.msg }
func (e *errWithCause) Unwrap() error { return e.cause }

func TestTryAcquireSessionLock_StaleLockIsCleared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks", "session-audit-A.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	// Write a lock file with an old mtime (simulates a dead holder).
	require.NoError(t, os.WriteFile(path, []byte("99999\n"), 0o644))
	staleTime := time.Now().Add(-(lockStaleDuration + time.Second))
	require.NoError(t, os.Chtimes(path, staleTime, staleTime))

	// Should succeed despite the existing file because it is stale.
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	require.NotNil(t, lk)
	require.NoError(t, lk.Release())
}

// TestTryAcquireSessionLock_OrphanPIDIsCleared covers the round-2 #12
// orphan-PID fast path: a lock whose holder PID is no longer alive
// must be reclaimable without waiting the full 20s stale window. We
// fake an orphan by writing a guaranteed-dead PID (a very high number)
// and back-dating mtime past the heartbeat tick.
func TestTryAcquireSessionLock_OrphanPIDIsCleared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks", "session-orphan.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	// PID 0x7FFFFFFE is virtually guaranteed not to be a running process.
	require.NoError(t, os.WriteFile(path, []byte("2147483646\n"), 0o644))

	// mtime older than one heartbeat tick (so the PID-check branch fires)
	// but YOUNGER than lockStaleDuration (so the existing mtime branch does
	// NOT cover it — the orphan-PID branch is the only thing that can
	// reclaim it).
	mtime := time.Now().Add(-(lockHeartbeatInterval + time.Second))
	require.NoError(t, os.Chtimes(path, mtime, mtime))

	lk, err := TryAcquireSessionLock(dir, "orphan")
	require.NoError(t, err, "orphan lock with dead PID should be reclaimed without waiting for the 20s stale window")
	require.NotNil(t, lk)
	require.NoError(t, lk.Release())
}

// TestTryAcquireSessionLock_StaleMtimeButNoRealLockIsReclaimed replaces the
// old removeIfStale-based test. Under the new protocol, a stale-looking
// mtime by itself proves nothing — reclaim is decided solely by attempting
// the real OS lock. Here nobody actually holds the OS lock (the file was
// written by a plain os.WriteFile, not through TryAcquireSessionLock), so
// even though the PID in the file is our own (definitely "alive"), the
// lock must still be acquirable: liveness of the PID is not authoritative
// either, only the OS lock attempt is. This exercises
// logStaleDiagnostics + acquireSessionLockFile end-to-end without ever
// calling the old removeIfStale/reclaimStaleLock internals, which no
// longer exist.
func TestTryAcquireSessionLock_StaleMtimeButNoRealLockIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks", "session-live.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	// Past the PID-check threshold but well before the 20s mtime threshold.
	mtime := time.Now().Add(-(lockHeartbeatInterval + time.Second))
	require.NoError(t, os.Chtimes(path, mtime, mtime))

	// Nobody actually holds the OS lock on this file (it was never
	// acquired via acquireSessionLockFile), so the real lock attempt
	// must succeed regardless of what the PID/mtime heuristics say.
	lk, err := TryAcquireSessionLock(dir, "live")
	require.NoError(t, err)
	require.NotNil(t, lk)
	require.NoError(t, lk.Release())
}

func TestTryAcquireSessionLock_FreshLockIsRespected(t *testing.T) {
	dir := t.TempDir()
	// Acquire a real lock so the file is fresh and OS-locked.
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	defer lk.Release()

	// A second acquire must fail — the file is fresh (heartbeat running).
	_, err = TryAcquireSessionLock(dir, "audit-A")
	var busyErr *SessionLockBusyError
	assert.True(t, errors.As(err, &busyErr), "expected SessionLockBusyError, got %v", err)
}

func TestHeartbeatTouchesFile(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)

	info1, err := os.Stat(lk.Path)
	require.NoError(t, err)
	before := info1.ModTime()

	// Wait slightly longer than one heartbeat tick.
	time.Sleep(lockHeartbeatInterval + 2*time.Second)

	info2, err := os.Stat(lk.Path)
	require.NoError(t, err)
	assert.True(t, info2.ModTime().After(before), "heartbeat must have touched the file")

	require.NoError(t, lk.Release())
}

func TestLockPathStructure(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	defer lk.Release()

	// Locks must live under <dataDir>/locks/ so they're easy to clean
	// up wholesale and don't pollute the data dir.
	expectedDir := filepath.Join(dir, "locks")
	assert.True(t, strings.HasPrefix(lk.Path, expectedDir),
		"lock file %q must be under %q", lk.Path, expectedDir)
	assert.True(t, strings.HasSuffix(lk.Path, ".lock"))
}

// assertBusyHolderPID checks the HolderPID reported in a SessionLockBusyError
// against the real holder's PID, accounting for a documented Windows-only
// limitation: tryLockFile's LockFileEx takes a MANDATORY lock on the whole
// file for the holder's lifetime, so a contending process's os.ReadFile of
// that same file (which is exactly what readLockHolderPID does) fails with
// a sharing/lock violation and reports PID 0 — not because the PID is
// unknown, but because Windows won't let a second handle read the range
// while the mandatory lock is held. See readLockFile's doc comment. POSIX
// advisory locks don't have this problem, so on non-Windows we assert the
// exact PID.
func assertBusyHolderPID(t *testing.T, wantPID, gotPID int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		assert.True(t, gotPID == 0 || gotPID == wantPID,
			"on Windows, HolderPID is expected to read back as 0 while the mandatory lock is held (or, if read timing allows it, the real PID); got %d, holder is %d", gotPID, wantPID)
		return
	}
	assert.Equal(t, wantPID, gotPID)
}

// ---------------------------------------------------------------------
// Real second-process regression tests for the reclaim protocol.
//
// These are the tests that actually prove the fix: a single Go test
// process taking flock twice on its own file descriptors is not a valid
// proxy for "does a second process see contention", and it is exactly
// that gap that let the original bug (unlink-by-mtime racing a live
// holder) ship. See lock_helper_test.go for the child-process harness.
// ---------------------------------------------------------------------

// TestCrossProcess_BusyIsImmediate is test A: a real second process
// holds the lock; our attempt to acquire the same session id must fail
// with SessionLockBusyError immediately (well under the 20s stale
// window), never falling through to any mtime-based wait.
func TestCrossProcess_BusyIsImmediate(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	holder := spawnLockHolder(t, dir, "cross-a", 0 /* hold until stopped */)
	defer holder.stop(t)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)

	start := time.Now()
	_, err := TryAcquireSessionLock(dir, "cross-a")
	elapsed := time.Since(start)

	var busyErr *SessionLockBusyError
	require.Error(t, err)
	require.True(t, errors.As(err, &busyErr), "expected SessionLockBusyError, got %v", err)
	assertBusyHolderPID(t, holder.pid, busyErr.HolderPID)
	assert.Less(t, elapsed, 5*time.Second,
		"acquire must fail fast via a real OS-lock contention check, not wait out any mtime timeout")
}

// TestCrossProcess_StaleMtimeButAliveHolderStaysBusy is test B: this is
// THE regression test for the bug itself. A real second process holds
// the lock and is genuinely alive and still holding the OS lock, but we
// artificially back-date the lock file's mtime past lockStaleDuration to
// simulate a lagging/failed heartbeat (GC pause, transient Chtimes
// failure, slow filesystem, etc). The lock must NOT be reclaimed: the
// old mtime-driven removeIfStale/reclaimStaleLock code would have
// unlinked the file here and let us "steal" the session out from under
// a live holder — two owners of one session id. The new protocol must
// refuse, because attempting the real OS lock still finds it held.
func TestCrossProcess_StaleMtimeButAliveHolderStaysBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	holder := spawnLockHolder(t, dir, "cross-b", 0 /* hold until stopped */)
	defer holder.stop(t)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)

	path := lockPathFor(dir, "cross-b")
	staleTime := time.Now().Add(-(lockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(path, staleTime, staleTime),
		"back-dating mtime to simulate a lagging heartbeat on an otherwise-live holder")

	// Sanity: the holder process is still alive and still holds the OS
	// lock at this point (we haven't stopped it).
	require.True(t, IsProcessAlive(holder.pid), "helper process must still be alive for this test to be meaningful")

	_, err := TryAcquireSessionLock(dir, "cross-b")
	var busyErr *SessionLockBusyError
	require.Error(t, err, "a live holder's lock must survive a stale-looking mtime — this is the core regression test for the reclaim bug")
	require.True(t, errors.As(err, &busyErr), "expected SessionLockBusyError, got %v", err)
	assertBusyHolderPID(t, holder.pid, busyErr.HolderPID)

	// Lock file must still be on disk and must NOT have been unlinked —
	// unlinking while the OS lock is held by a live process is exactly
	// the bug: flock is bound to the inode, so unlink doesn't revoke the
	// live holder's lock, it just lets a new inode appear at the same
	// path and create a second "owner".
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "lock file for a live, still-locking holder must not be removed")
}

// TestCrossProcess_SubAgentChildSessionIsProtected is the regression test
// for P1.1 (docs/reviews/2026-07-30-project-review.md): a sub-agent runs
// under its own CHILD session id (format "parentMessageID$$toolCallID", see
// session.CreateAgentToolSessionID / agent.go's runSubAgent), which is a
// completely different id from its parent's session id. Before the fix,
// agent.Run's inter-process lock was skipped entirely for sub-agents
// (`if !a.isSubAgent && a.dataDir != ""`), so nothing ever locked the child
// session id at the OS level — a second crush process opening that exact
// child session id (e.g. via `crush sessions pick`/`resume`) could acquire
// it and stream into it concurrently with the in-process sub-agent run.
//
// This test doesn't invoke the agent package (that would require a full
// fantasy.Agent harness); it proves the lock primitive itself protects a
// child-shaped session id exactly the same way it protects a top-level one,
// which is what agent.Run now relies on now that the isSubAgent exemption is
// gone. A real second process holds the lock on the child id; our attempt
// to acquire the same id must fail with SessionLockBusyError, exactly like
// TestCrossProcess_BusyIsImmediate proves for a plain top-level session id.
func TestCrossProcess_SubAgentChildSessionIsProtected(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	// Shaped exactly like session.Service.CreateAgentToolSessionID's output:
	// fmt.Sprintf("%s$$%s", messageID, toolCallID).
	childSessionID := "msg-parent-123$$toolcall-abc"

	holder := spawnLockHolder(t, dir, childSessionID, 0 /* hold until stopped */)
	defer holder.stop(t)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)

	start := time.Now()
	_, err := TryAcquireSessionLock(dir, childSessionID)
	elapsed := time.Since(start)

	var busyErr *SessionLockBusyError
	require.Error(t, err, "a second process must not be able to acquire the lock for an active sub-agent's child session id")
	require.True(t, errors.As(err, &busyErr), "expected SessionLockBusyError, got %v", err)
	assertBusyHolderPID(t, holder.pid, busyErr.HolderPID)
	assert.Less(t, elapsed, 5*time.Second,
		"acquire must fail fast via a real OS-lock contention check, not wait out any mtime timeout")
}

// TestCrossProcess_DeadHolderIsReclaimedPromptly is test C: a real
// second process acquires the lock, then is killed (SIGKILL / TerminateProcess)
// without any explicit unlock or cleanup. The OS itself releases the
// flock/LockFileEx when the process dies, so our next acquire attempt
// must succeed promptly via the real lock attempt — it must not need to
// wait for the mtime staleness window to elapse, and it must not rely on
// unlinking the old file (the file's own mtime here is deliberately left
// FRESH, to prove reclaim happens via the OS lock, not via any mtime
// heuristic).
func TestCrossProcess_DeadHolderIsReclaimedPromptly(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	holder := spawnLockHolder(t, dir, "cross-c", 0 /* hold until stopped */)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)

	// Kill without giving it a chance to Release() — simulates a crash.
	require.NoError(t, holder.cmd.Process.Kill())
	_, _ = holder.cmd.Process.Wait()

	// Mtime is left exactly as the (still-alive-when-it-wrote-it) holder
	// last touched it — i.e. fresh, well inside lockStaleDuration. If
	// reclaim were still mtime-gated, this acquire would incorrectly
	// fail as "busy" for up to 20s. It must instead succeed immediately
	// because the OS lock attempt is what decides, and the OS released
	// the dead process's lock already.
	start := time.Now()
	lk, err := TryAcquireSessionLock(dir, "cross-c")
	elapsed := time.Since(start)

	require.NoError(t, err, "a lock abandoned by a killed process must be reclaimable via the real OS lock, without waiting for mtime to go stale")
	require.NotNil(t, lk)
	assert.Less(t, elapsed, 5*time.Second,
		"reclaim of a dead holder's lock must happen promptly via OS lock, not via any mtime wait")
	require.NoError(t, lk.Release())
}
