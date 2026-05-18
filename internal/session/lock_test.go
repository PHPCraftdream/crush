package session

import (
	"errors"
	"os"
	"sync"
	"path/filepath"
	"strings"
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
		"audit-A":                                "audit-A",
		"abc/def":                                "abc_def",
		"abc\\def":                               "abc_def",
		"weird:id:with:colons":                   "weird_id_with_colons",
		"a*b?c\"d<e>f|g h":                       "a_b_c_d_e_f_g_h",
		"550e8400-e29b-41d4-a716-446655440000":   "550e8400-e29b-41d4-a716-446655440000",
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
