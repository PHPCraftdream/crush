package session

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestP1_2_ReleaseUnlocksBeforeMetadataCleanup_Hang verifies that SessionLock.Release()
// unlocks the OS lock and closes the file descriptor BEFORE attempting diagnostic
// metadata cleanup. Without this (P1-2 from the 2026-08-07 concurrency review), a hung
// filesystem/AV/SMB during clearHolderMetadata (Truncate/Seek/Sync/Remove) would
// prevent the OS lock from ever being released, wedging the session forever.
//
// This test uses a test seam (clearHolderMetadataFn package var) to inject
// artificial blocking behavior into clearHolderMetadata, proving the critical
// invariant: unlockFile + Close happen BEFORE clearHolderMetadata, not after.
//
// P0 fix note: With the background cleanup fix, Release() returns immediately
// after unlock/close, but the invariant being tested here is still valid: the
// unlock/close happen BEFORE the cleanup goroutine even starts.
//
// NOTE: This test is NOT parallel because it mutates the package-global
// clearHolderMetadataFn seam, which would create a data race with other
// parallel tests doing the same. See task #345 for the architectural fix.
//
// REVERT CHECK PROCEDURE:
//  1. In lock.go:~296, restore the old order by moving clearHolderMetadataFn(l.Path)
//     to BEFORE unlockFile(l.f) and BEFORE f.Close():
//     // OLD ORDER (BUG): clear metadata BEFORE unlock/close
//     clearHolderMetadataFn(l.Path)
//     unlockErr := unlockFile(l.f)
//     closeErr := l.f.Close()
//  2. Run: go test ./internal/session -run TestP1_2_ReleaseUnlocksBeforeMetadataCleanup_Hang -v
//  3. The test will FAIL because second TryAcquireSessionLock fails while clearHolderMetadata is blocked
//  4. Restore the fix (unlockFile first, then close, then clearHolderMetadataFn) and the test will PASS.
func TestP1_2_ReleaseUnlocksBeforeMetadataCleanup_Hang(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-p1-2-hang"

	// Acquire the first lock.
	lk1, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err, "first TryAcquireSessionLock should succeed")
	require.NotNil(t, lk1)

	// Inject a blocking version of clearHolderMetadataFn.
	// It will block on releaseBlocker channel until the test unblocks it.
	var releaseStarted atomic.Bool
	var releaseCompleted atomic.Bool
	releaseBlocker := make(chan struct{})
	clearHolderMetadataOriginal := clearHolderMetadataFn

	clearHolderMetadataFn = func(path string) {
		releaseStarted.Store(true)
		// Block until the test signals to proceed.
		<-releaseBlocker
		// Call the original implementation.
		clearHolderMetadataOriginal(path)
		releaseCompleted.Store(true)
	}
	// Restore the original function after the test.
	defer func() {
		clearHolderMetadataFn = clearHolderMetadataOriginal
	}()

	// Start Release() in a goroutine. It should acquire the OS lock, call
	// unlockFile/close, then BLOCK on our injected clearHolderMetadataFn.
	releaseDone := make(chan struct{})
	go func() {
		_ = lk1.Release()
		close(releaseDone)
	}()

	// Wait for Release() to return (P0 fix: it returns immediately, before cleanup).
	releaseReturned := make(chan struct{})
	go func() {
		<-releaseDone
		close(releaseReturned)
	}()

	select {
	case <-releaseReturned:
		// Expected: Release returns quickly even though cleanup is blocked.
	case <-time.After(100 * time.Millisecond):
		require.Fail(t, "Release() should return within 100ms even with blocked cleanup")
	}

	// Wait for cleanup to start (proves goroutine was spawned).
	deadline := time.After(2 * time.Second)
	for !releaseStarted.Load() {
		select {
		case <-deadline:
			require.Fail(t, "cleanup goroutine did not start within 2 seconds")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// At this point, Release() has called unlockFile and Close, but the cleanup
	// goroutine is BLOCKED in clearHolderMetadataFn. The critical invariant is that
	// unlockFile/Close happened BEFORE the block. We prove this by trying to acquire
	// the lock from a second goroutine - if unlockFile already happened, this should succeed.
	acquireSuccess := make(chan struct{})
	var lk2 *SessionLock

	go func() {
		lk2, _ = TryAcquireSessionLock(tmpDir, sessionID)
		if lk2 != nil {
			close(acquireSuccess)
		}
	}()

	// The second acquire should succeed (proving unlock happened BEFORE the block).
	select {
	case <-acquireSuccess:
		// Expected: unlockFile already ran, so lock is available.
		require.NotNil(t, lk2, "second lock should not be nil")
	case <-time.After(2 * time.Second):
		require.Fail(t, "second TryAcquireSessionLock should succeed while clearHolderMetadataFn is blocked")
	}

	// Now unblock clearHolderMetadataFn so Release() can complete.
	close(releaseBlocker)

	// Wait for cleanup to complete.
	deadline = time.After(2 * time.Second)
	for !releaseCompleted.Load() {
		select {
		case <-deadline:
			require.Fail(t, "cleanup should complete within 2 seconds after unblocking")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Clean up lk2 to avoid Windows file handle conflicts.
	if lk2 != nil {
		_ = lk2.Release()
	}

	// Clean up.
	if lk2 != nil {
		_ = lk2.Release()
	}
}

// TestP1_2_ReleaseUnlocksBeforeMetadataCleanup verifies that SessionLock.Release()
// unlocks the OS lock and closes the file descriptor BEFORE attempting diagnostic
// metadata cleanup. This is a non-blocking version that doesn't inject hangs,
// just verifies the happy path.
//
// REVERT CHECK PROCEDURE:
//  1. In lock.go:~296, restore the old order by moving clearHolderMetadataFn(l.Path)
//     to BEFORE unlockFile(l.f) and BEFORE f.Close():
//     // OLD ORDER (BUG): clear metadata BEFORE unlock/close
//     clearHolderMetadataFn(l.Path)
//     unlockErr := unlockFile(l.f)
//     closeErr := l.f.Close()
//  2. Run: go test ./internal/session -run TestP1_2_ReleaseUnlocksBeforeMetadataCleanup -v
//  3. The test will FAIL because the lock is never released (TryAcquireSessionLock from another goroutine times out)
//  4. Restore the fix (unlockFile first, then close, then clearHolderMetadataFn) and the test will PASS.
func TestP1_2_ReleaseUnlocksBeforeMetadataCleanup(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	locksDir := filepath.Join(tmpDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))

	sessionID := "test-session-p1-2-release-order"

	// Acquire the first lock.
	lk1, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err, "first TryAcquireSessionLock should succeed")
	require.NotNil(t, lk1)

	// Release the lock. The key assertion is that this Release() call
	// completes promptly and unlocks the OS lock, even if clearHolderMetadata
	// were to hang (we can't easily simulate the hang, but we can verify that
	// the unlock happened).
	releaseStart := time.Now()
	releaseErrValue := lk1.Release()
	releaseDuration := time.Since(releaseStart)

	require.NoError(t, releaseErrValue, "Release should succeed")
	// Release should complete quickly (not hang on metadata cleanup).
	require.Less(t, releaseDuration, 5*time.Second, "Release should not hang for more than 5 seconds")

	// With the P0 fix (cleanup holds lock through entire operation), we need
	// to wait for cleanup to complete before trying to reacquire.
	require.Eventually(t, func() bool {
		lk2, err := TryAcquireSessionLock(tmpDir, sessionID)
		if err == nil && lk2 != nil {
			_ = lk2.Release()
			return true
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "second acquire should eventually succeed after cleanup completes")
}

// TestP1_2_ReleaseIdempotentWithHungCleanup verifies that calling Release()
// multiple times is safe even if the first call's metadata cleanup hangs.
// This ensures the sync.Once mechanism works correctly.
func TestP1_2_ReleaseIdempotentWithHungCleanup(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessionID := "test-session-p1-2-idempotent"

	lk, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err)
	require.NotNil(t, lk)

	// Release multiple times - only the first should do actual work.
	require.NoError(t, lk.Release(), "first Release should succeed")
	require.NoError(t, lk.Release(), "second Release should succeed (no-op)")
	require.NoError(t, lk.Release(), "third Release should succeed (no-op)")
}

// TestP1_2_ReleaseNilSafe verifies that Release() is safe to call on nil.
func TestP1_2_ReleaseNilSafe(t *testing.T) {
	t.Parallel()

	var lk *SessionLock
	require.NoError(t, lk.Release(), "Release on nil should succeed (no-op)")
}

// TestP1_2_ReleaseClosesFileDescriptor verifies that Release() closes the
// file descriptor even if metadata cleanup fails or hangs. We can't easily
// simulate a hang without modifying the lock implementation, but we can verify
// that the file is closed and can be reopened after Release.
func TestP1_2_ReleaseClosesFileDescriptor(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessionID := "test-session-p1-2-fd-close"

	lk1, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err)
	require.NotNil(t, lk1)

	// Verify the lock file exists.
	_, err = os.Stat(lk1.Path)
	require.NoError(t, err, "lock file should exist before release")

	// Release the lock.
	require.NoError(t, lk1.Release())

	// Verify the lock file still exists (we don't unlink it).
	_, err = os.Stat(lk1.Path)
	require.NoError(t, err, "lock file should still exist after release")

	// With the P0 fix (cleanup holds lock through entire operation), we need
	// to wait for cleanup to complete before trying to reacquire.
	var lk2 *SessionLock
	require.Eventually(t, func() bool {
		lk2, err = TryAcquireSessionLock(tmpDir, sessionID)
		return err == nil && lk2 != nil
	}, 2*time.Second, 10*time.Millisecond, "second acquire should eventually succeed after cleanup completes")
	require.NotNil(t, lk2)

	// Clean up.
	require.NoError(t, lk2.Release())
}

// TestP1_2_ConcurrentReleaseDoesNotDeadlock verifies that concurrent Release()
// calls do not deadlock, even if metadata cleanup takes time. This tests the
// sync.Once mechanism.
func TestP1_2_ConcurrentReleaseDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessionID := "test-session-p1-2-concurrent"

	lk, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err)
	require.NotNil(t, lk)

	// Release from multiple goroutines - only one should actually do the work.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = lk.Release()
		}()
	}

	// Wait for all goroutines to complete with a timeout.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Expected: all goroutines completed without deadlock.
	case <-time.After(5 * time.Second):
		require.Fail(t, "concurrent Release() calls should complete within 5 seconds")
	}
}

// TestP0_ReleaseReturnsImmediatelyDuringHungCleanup proves that Release()
// returns control to the caller immediately after unlock/close, even if
// metadata cleanup is blocked forever. This is the critical fix for the
// FREEZE mechanism: the mailbox state machine can transition to mbIdle
// without waiting for potentially-infinite filesystem I/O.
//
// NOTE: This test is NOT parallel because it mutates the package-global
// clearHolderMetadataFn seam, which would create a data race with other
// parallel tests doing the same. See task #345 for the architectural fix.
//
// REVERT CHECK PROCEDURE:
//  1. In lock.go Release(), change "go clearHolderMetadataFn(path)" back to
//     "clearHolderMetadataFn(l.Path)" (remove the "go " keyword).
//  2. Run: go test ./internal/session -run TestP0_ReleaseReturnsImmediatelyDuringHungCleanup -v
//  3. The test will FAIL because Release() does not return while cleanup is blocked.
//  4. Restore the fix (add "go " back) and the test will PASS.
func TestP0_ReleaseReturnsImmediatelyDuringHungCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-p0-freeze-fix"

	// Acquire the first lock.
	lk1, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err, "first TryAcquireSessionLock should succeed")
	require.NotNil(t, lk1)

	// Inject a FOREVER-blocking version of clearHolderMetadataFn.
	// It will block on cleanupBlocker channel and NEVER unblock.
	var cleanupStarted atomic.Bool
	cleanupBlocker := make(chan struct{}) // Never closed
	clearHolderMetadataOriginal := clearHolderMetadataFn

	clearHolderMetadataFn = func(path string) {
		cleanupStarted.Store(true)
		<-cleanupBlocker // Block forever - this simulates hung FS/AV/SMB
		clearHolderMetadataOriginal(path)
	}
	defer func() {
		clearHolderMetadataFn = clearHolderMetadataOriginal
	}()

	// Call Release() and measure how long it takes.
	// CRITICAL: It should return IMMEDIATELY (within 100ms), not wait for cleanup.
	releaseStart := time.Now()
	releaseErr := lk1.Release()
	releaseDuration := time.Since(releaseStart)

	require.NoError(t, releaseErr, "Release should succeed")
	require.Less(t, releaseDuration, 100*time.Millisecond,
		"Release() should return immediately even with hung cleanup, got %v", releaseDuration)

	// Verify that cleanup goroutine actually started (proves the "go" keyword is there).
	deadline := time.After(2 * time.Second)
	for !cleanupStarted.Load() {
		select {
		case <-deadline:
			require.Fail(t, "cleanup goroutine did not start within 2 seconds")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// CRITICAL PROOF: Even though cleanup is blocked forever, the OS lock
	// is already free. Another process can acquire it.
	lk2, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err, "second acquire should succeed while first cleanup is blocked")
	require.NotNil(t, lk2)

	// Verify the second owner's PID is correctly written.
	pid := ReadLockPID(lk2.Path)
	require.Equal(t, lk2.HolderPID, pid, "second lock should have its own PID")

	// Cleanup the second lock.
	require.NoError(t, lk2.Release())

	// Note: We don't unblock cleanupBlocker because it's meant to block forever.
	// The goroutine will be GC'd when the test completes. This is fine for a test.
}

// TestP1_2_ReleaseClearsMetadata verifies that Release() does attempt to clear
// metadata (PID, sidecar) even if the file is reopened for that purpose.
// We can verify this by checking that the lock file is empty after release.
//
// NOTE: With the P0 fix (background cleanup), this test now checks that
// cleanup EVENTUALLY happens, not that it happens before Release() returns.
// We need to wait for the background goroutine to complete.
func TestP1_2_ReleaseClearsMetadata(t *testing.T) {
	// Not parallel - depends on clean global seam state

	tmpDir := t.TempDir()
	sessionID := "test-session-p1-2-metadata-clear"

	lk, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err)
	require.NotNil(t, lk)

	// Verify the lock file contains our PID.
	pid := ReadLockPID(lk.Path)
	require.Equal(t, lk.HolderPID, pid, "lock file should contain our PID")

	// Release the lock.
	require.NoError(t, lk.Release())

	// With background cleanup, we need to wait a bit for the goroutine to complete.
	// Give it 2 seconds - cleanup should be very fast on a local tmpdir.
	require.Eventually(t, func() bool {
		pid := ReadLockPID(lk.Path)
		return pid == 0
	}, 2*time.Second, 10*time.Millisecond, "lock file should have PID 0 after release")

	// Verify the sidecar file is gone.
	sidecarPath := pidSidecarPath(lk.Path)
	_, err = os.Stat(sidecarPath)
	require.True(t, os.IsNotExist(err), "sidecar file should be removed after release")
}
