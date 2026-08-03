package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// After a clean Release, the lock FILE stays on disk (see
	// acquireSessionLockFile/package doc for why unlinking the path is
	// unsafe), but the PID metadata it carried must be gone — see
	// Release's doc comment. A cleanly-exited holder must not leave a
	// PID behind that a later `sessions kill` could misread as a live
	// holder once the OS recycles that PID number for an unrelated
	// process (see clearHolderMetadata).
	require.NoError(t, lk.Release())
	assert.Equal(t, 0, ReadLockPID(lk.Path),
		"after release, the lock file/sidecar must not carry the old holder's PID")
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

	// The heartbeat is gated on real activity (see RecordActivity's doc
	// comment and #213) — with no recorded activity, the tick is skipped.
	// Record activity so this tick is expected to touch the file; the
	// no-activity case is covered separately by
	// TestHeartbeat_NoActivity_DoesNotTouchMtime.
	lk.RecordActivity()

	// Wait slightly longer than one heartbeat tick.
	time.Sleep(lockHeartbeatInterval + 2*time.Second)

	info2, err := os.Stat(lk.Path)
	require.NoError(t, err)
	assert.True(t, info2.ModTime().After(before), "heartbeat must have touched the file when activity was recorded")

	require.NoError(t, lk.Release())
}

// TestHeartbeat_NoActivity_DoesNotTouchMtime is the core regression test
// for #213: gate the heartbeat on real activity, not a blind timer. If
// RecordActivity is never called, a full heartbeat interval must pass
// with the lock file's mtime untouched — a genuinely wedged holder (no
// forward progress) must stop looking alive to diagnostics, instead of
// the old behavior where a plain ticker kept touching mtime forever
// regardless of whether the process was making progress.
func TestHeartbeat_NoActivity_DoesNotTouchMtime(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	defer lk.Release()

	info1, err := os.Stat(lk.Path)
	require.NoError(t, err)
	before := info1.ModTime()

	// Deliberately do NOT call lk.RecordActivity(). Wait slightly longer
	// than one heartbeat tick so the ticker branch has definitely fired
	// at least once.
	time.Sleep(lockHeartbeatInterval + 2*time.Second)

	info2, err := os.Stat(lk.Path)
	require.NoError(t, err)
	assert.Equal(t, before, info2.ModTime(),
		"heartbeat must NOT touch the lock file's mtime when no activity was recorded during the interval")
}

// TestHeartbeat_RecordActivity_TouchesMtimeOnNextTick proves the positive
// case of the same gate: calling RecordActivity at any point during an
// interval causes the NEXT tick to touch mtime, exactly like the old
// unconditional behavior did — the gate only removes updates during
// genuinely idle intervals, it doesn't break the live/active case.
func TestHeartbeat_RecordActivity_TouchesMtimeOnNextTick(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	defer lk.Release()

	info1, err := os.Stat(lk.Path)
	require.NoError(t, err)
	before := info1.ModTime()

	// Record activity partway through the interval, simulating a turn
	// loop calling RecordActivity() as it makes progress.
	time.Sleep(1 * time.Second)
	lk.RecordActivity()

	time.Sleep(lockHeartbeatInterval + 2*time.Second)

	info2, err := os.Stat(lk.Path)
	require.NoError(t, err)
	assert.True(t, info2.ModTime().After(before),
		"heartbeat must touch the lock file's mtime on the next tick after RecordActivity was called")
}

// TestRecordActivity_ConcurrentSafe proves RecordActivity is safe to call
// from many goroutines simultaneously, without racing the heartbeat
// goroutine that concurrently reads/resets the same flag. This matters
// because #214 wires RecordActivity in from both the agent's turn loop
// and cross-goroutine watchdog callbacks — never a single well-defined
// caller. Run with -race to be meaningful.
func TestRecordActivity_ConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "audit-A")
	require.NoError(t, err)
	defer lk.Release()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				lk.RecordActivity()
			}
		}()
	}
	wg.Wait()
}

// TestInspectSessionLock_FreshMtimeIsLiveFastPath proves the existing
// "mtime fresh" fast path is unaffected by the task #228 PID-fallback
// change: immediately after acquiring a lock, mtime is fresh, so Live must
// be true without needing (or attempting) any PID probe.
func TestInspectSessionLock_FreshMtimeIsLiveFastPath(t *testing.T) {
	dir := t.TempDir()
	lk, err := TryAcquireSessionLock(dir, "inspect-fresh")
	require.NoError(t, err)
	defer lk.Release()

	st := InspectSessionLock(dir, "inspect-fresh", externalOwnerLiveThresholdForTest)
	assert.True(t, st.Exists)
	assert.True(t, st.Live, "a freshly acquired lock's mtime must be well within the live threshold")
	assert.Equal(t, os.Getpid(), st.PID)
}

// TestInspectSessionLock_StaleMtimeButLivePIDIsStillLive is the core
// regression test for task #228: a lock's heartbeat mtime is now gated on
// real RecordActivity() calls (task #213/#214), and the stream watchdog
// that supplies those calls while a tool is in flight only fires roughly
// every 30s (task #222) — larger than the 20s liveThreshold most callers
// pass. That leaves a real window where a perfectly healthy, tool-busy
// session looks mtime-stale even though it's still genuinely running. A
// real second process holds the lock (spawnLockHolder, mirroring the
// pattern used throughout this file / sessions_kill_test.go's
// spawnKillTestLockHolder) so the PID-liveness fallback is exercised
// against a genuinely live OS process, not a same-process fake. We
// back-date the lock file's mtime past the threshold to simulate the
// heartbeat lagging behind a long tool call, then assert
// InspectSessionLock still reports Live: true via the PID fallback.
func TestInspectSessionLock_StaleMtimeButLivePIDIsStillLive(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	holder := spawnLockHolder(t, dir, "inspect-stale-live", 0 /* hold until stopped */)
	defer holder.stop(t)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)

	path := lockPathFor(dir, "inspect-stale-live")
	staleTime := time.Now().Add(-(externalOwnerLiveThresholdForTest + 5*time.Second))
	require.NoError(t, os.Chtimes(path, staleTime, staleTime),
		"back-dating mtime to simulate a heartbeat lagging behind one long tool call on an otherwise-healthy session")

	require.True(t, IsProcessAlive(holder.pid), "helper process must still be alive for this test to be meaningful")

	st := InspectSessionLock(dir, "inspect-stale-live", externalOwnerLiveThresholdForTest)
	assert.True(t, st.Exists)
	assert.Equal(t, holder.pid, st.PID)
	assert.True(t, st.Live, "a stale mtime must not override a genuinely live PID holder — this is the core #228 fix")
}

// TestInspectSessionLock_StaleMtimeBeyondMaxPidFallbackAgeIsNotLive is the
// core regression test for task #235: task #228's PID-liveness fallback has
// no time bound of its own, so a lock left behind by a killed/crashed
// holder would report Live: true forever the moment the OS happened to
// recycle that exact PID number for some unrelated, currently-running
// process. This proves the maxPidFallbackAge bound closes that gap: once
// the lock's mtime is older than maxPidFallbackAge, InspectSessionLock must
// fall back to mtime-only liveness (Live: false) even though the recorded
// PID currently belongs to a genuinely live OS process (a real second
// process, via spawnLockHolder, standing in for "the OS reused this exact
// PID number").
func TestInspectSessionLock_StaleMtimeBeyondMaxPidFallbackAgeIsNotLive(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	holder := spawnLockHolder(t, dir, "inspect-stale-beyond-bound", 0 /* hold until stopped */)
	defer holder.stop(t)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)

	path := lockPathFor(dir, "inspect-stale-beyond-bound")
	staleTime := time.Now().Add(-(maxPidFallbackAge + 5*time.Second))
	require.NoError(t, os.Chtimes(path, staleTime, staleTime),
		"back-dating mtime past maxPidFallbackAge to simulate a lock left behind long enough ago that its recorded PID can no longer be trusted, even though it currently resolves to a live process")

	require.True(t, IsProcessAlive(holder.pid), "helper process must still be alive for this test to be meaningful — proves the bound, not merely a dead PID")

	st := InspectSessionLock(dir, "inspect-stale-beyond-bound", externalOwnerLiveThresholdForTest)
	assert.True(t, st.Exists)
	assert.Equal(t, holder.pid, st.PID)
	assert.False(t, st.Live, "a lock older than maxPidFallbackAge must fall back to mtime-only liveness even when its recorded PID is currently alive — this is the core #235 fix")
}

// TestInspectSessionLock_StaleMtimeAroundMaxPidFallbackAgeBoundary is a
// boundary companion to the test above: just under maxPidFallbackAge past
// staleness, the PID fallback must still apply (Live: true); just over it,
// the bound must kick in (Live: false). Exercises the exact `age <
// maxPidFallbackAge` comparison in InspectSessionLock.
func TestInspectSessionLock_StaleMtimeAroundMaxPidFallbackAgeBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	holder := spawnLockHolder(t, dir, "inspect-boundary", 0 /* hold until stopped */)
	defer holder.stop(t)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)

	path := lockPathFor(dir, "inspect-boundary")

	justUnder := time.Now().Add(-(maxPidFallbackAge - time.Second))
	require.NoError(t, os.Chtimes(path, justUnder, justUnder))
	st := InspectSessionLock(dir, "inspect-boundary", externalOwnerLiveThresholdForTest)
	assert.True(t, st.Live, "just under maxPidFallbackAge past staleness, a live PID must still be trusted")

	justOver := time.Now().Add(-(maxPidFallbackAge + time.Second))
	require.NoError(t, os.Chtimes(path, justOver, justOver))
	st = InspectSessionLock(dir, "inspect-boundary", externalOwnerLiveThresholdForTest)
	assert.False(t, st.Live, "just over maxPidFallbackAge past staleness, a live PID must no longer be trusted")
}

// TestInspectSessionLock_StaleMtimeAndDeadPIDIsNotLive is the conservative
// companion to the fix above: when mtime is stale AND the recorded PID is
// genuinely dead, InspectSessionLock must still report Live: false. The
// PID-fallback must not regress the correct "actually dead" case into a
// false positive.
func TestInspectSessionLock_StaleMtimeAndDeadPIDIsNotLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks", "session-inspect-stale-dead.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	// PID 0x7FFFFFFE is virtually guaranteed not to be a running process
	// (same sentinel TestTryAcquireSessionLock_OrphanPIDIsCleared uses).
	require.NoError(t, os.WriteFile(path, []byte("2147483646\n"), 0o644))
	staleTime := time.Now().Add(-(externalOwnerLiveThresholdForTest + 5*time.Second))
	require.NoError(t, os.Chtimes(path, staleTime, staleTime))

	st := InspectSessionLock(dir, "inspect-stale-dead", externalOwnerLiveThresholdForTest)
	assert.True(t, st.Exists)
	assert.False(t, st.Live, "a stale mtime with a genuinely dead PID must still report Live: false")
}

// TestInspectSessionLock_StaleMtimeAndUnreadablePIDIsNotLive covers the
// sibling conservative case: mtime is stale and the lock file has no
// readable PID at all (PID 0). There is nothing to probe, so this must
// fall straight through to Live: false, exactly like the pure-mtime
// behavior did before task #228.
func TestInspectSessionLock_StaleMtimeAndUnreadablePIDIsNotLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks", "session-inspect-stale-nopid.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	require.NoError(t, os.WriteFile(path, []byte(""), 0o644))
	staleTime := time.Now().Add(-(externalOwnerLiveThresholdForTest + 5*time.Second))
	require.NoError(t, os.Chtimes(path, staleTime, staleTime))

	st := InspectSessionLock(dir, "inspect-stale-nopid", externalOwnerLiveThresholdForTest)
	assert.True(t, st.Exists)
	assert.Equal(t, 0, st.PID)
	assert.False(t, st.Live, "a stale mtime with no readable PID must report Live: false")
}

// externalOwnerLiveThresholdForTest mirrors internal/server/handlers.go's
// externalOwnerLiveThreshold (20s) — the actual threshold InspectSessionLock
// is called with in production. Duplicated here (rather than imported) to
// avoid this low-level package depending on internal/server.
const externalOwnerLiveThresholdForTest = 20 * time.Second

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
// against the real holder's PID. Windows note: tryLockFile's LockFileEx
// takes a MANDATORY lock on the whole lock file for the holder's lifetime,
// so a contending process's os.ReadFile of that same file would normally
// fail with a sharing/lock violation and report PID 0 — not because the
// PID is unknown, but because Windows won't let a second handle read the
// range while the mandatory lock is held. TryAcquireSessionLock works
// around this by also stamping the PID into a never-locked sidecar file
// (see readLockFile's doc comment), so on every OS — including Windows —
// a busy error must now report the real holder PID.
func assertBusyHolderPID(t *testing.T, wantPID, gotPID int) {
	t.Helper()
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

// TestCrossProcess_ReadLockPIDWorksWhileHolderIsAlive is the direct
// regression test for the `crush sessions kill` failure this was written
// to fix: an operator ran `crush sessions kill <id>` against a genuinely
// live, still-running session on Windows and got "lock has no readable
// PID; removing file only" followed by a sharing-violation error trying
// to delete the still-open lock file — sessions kill could never identify
// (let alone kill) the exact class of holder it exists to kill.
//
// Root cause: on Windows, LockFileEx's mandatory whole-file lock makes a
// plain os.ReadFile of the primary lock file fail while a live holder has
// it open, so ReadLockPID (which sessions kill calls) always saw PID 0
// for a genuinely alive session — not just a dead/stale one. The fix
// stamps the same PID into a companion, never-locked sidecar file that a
// contending process CAN always read. This test spawns a real second
// process holding the lock and asserts ReadLockPID — the exact function
// `crush sessions kill` uses — returns its real PID while it is still
// alive and still holding the lock, on every OS.
func TestCrossProcess_ReadLockPIDWorksWhileHolderIsAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()

	holder := spawnLockHolder(t, dir, "kill-target", 0 /* hold until stopped */)
	defer holder.stop(t)
	require.True(t, holder.locked, "helper process failed to acquire lock: %s", holder.failed)
	require.True(t, IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	path := lockPathFor(dir, "kill-target")
	got := ReadLockPID(path)
	assert.Equal(t, holder.pid, got,
		"ReadLockPID (what `crush sessions kill` calls) must be able to name a genuinely live holder's PID, not just a dead one")
}
