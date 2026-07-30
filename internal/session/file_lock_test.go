package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsContentionErrorDetectsTypedError is a regression test for the
// switch away from substring matching ("file lock contended" in err.Error())
// to a typed *ErrLockContended + errors.As check. Before the fix, renaming
// the message text inside TryAcquireFileLock (an innocuous refactor) would
// silently break isContentionError's classification with nothing catching
// the mismatch — a genuine lock-contention error would stop being retried
// and instead fail AcquireFileLockContext immediately.
func TestIsContentionErrorDetectsTypedError(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	holder, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })

	_, err = TryAcquireFileLock(lockPath)
	require.Error(t, err, "second acquire on an already-held lock must fail")

	var contended *ErrLockContended
	assert.True(t, errors.As(err, &contended),
		"TryAcquireFileLock's contention error must be detectable via errors.As(*ErrLockContended), got: %v", err)
	assert.True(t, isContentionError(err), "isContentionError must classify a real contention error as retryable")
	assert.Equal(t, lockPath, contended.Path)
}

// TestIsContentionErrorRejectsNonContentionErrors proves isContentionError
// does not misclassify unrelated errors (e.g. a plain wrapped error, or nil)
// as retryable contention — only genuine *ErrLockContended must retry.
func TestIsContentionErrorRejectsNonContentionErrors(t *testing.T) {
	assert.False(t, isContentionError(nil))
	assert.False(t, isContentionError(errors.New("some unrelated failure")))
	assert.False(t, isContentionError(os.ErrPermission))
}

// TestErrLockContendedMessageChangeDoesNotBreakClassification is the direct
// regression scenario the fix targets: if a future refactor changes the
// wording TryAcquireFileLock uses for its contention error (previously a
// "file lock contended" substring elsewhere in the message would have been
// required for isContentionError's old strings.Contains check to work),
// classification must still work because it is now driven by the error's
// TYPE, not its text.
func TestErrLockContendedMessageChangeDoesNotBreakClassification(t *testing.T) {
	// Simulate an arbitrarily-worded wrap around ErrLockContended, as if
	// TryAcquireFileLock's message had been reworded away from any
	// "contended" substring.
	reworded := &ErrLockContended{Path: "/some/path"}
	wrapped := errAs(reworded)

	assert.True(t, isContentionError(wrapped),
		"classification must survive an arbitrary error message as long as the type is *ErrLockContended")
}

// errAs wraps err in a differently-worded error using the standard %w verb,
// simulating a future TryAcquireFileLock whose message text no longer
// contains any particular substring.
func errAs(err error) error {
	return &wrappedErr{msg: "totally different wording, no magic substring here", err: err}
}

type wrappedErr struct {
	msg string
	err error
}

func (w *wrappedErr) Error() string { return w.msg }
func (w *wrappedErr) Unwrap() error { return w.err }

// TestAcquireFileLockContextRetriesOnContentionAndTimesOut is a behavioral
// regression test: AcquireFileLockContext must keep retrying (not fail
// immediately) while the lock is held by another holder, and must return
// the context error once its deadline passes — proving isContentionError's
// classification is actually wired into the retry loop, not just unit-tested
// in isolation.
func TestAcquireFileLockContextRetriesOnContentionAndTimesOut(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	holder, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = AcquireFileLockContext(ctx, lockPath)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	// Must have actually retried (backoff starts at 25ms) rather than
	// bailing out on the first contention error.
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond,
		"AcquireFileLockContext returned too quickly — contention error was likely misclassified as fatal instead of retried")
}
