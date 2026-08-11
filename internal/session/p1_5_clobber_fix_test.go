package session

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestP1_5_GenerationCheckPreventsMetadataClobber verifies that a stale
// cleanup goroutine from a prior release does NOT clobber a new owner's
// metadata. This is the fix for P1-5 from the 2026-08-11 review.
//
// The test forces the clobber scenario by:
// 1. Injecting a slow cleanup function for the first holder (blocks >50ms)
// 2. Releasing the first holder (returns quickly due to bound, but cleanup is still blocked)
// 3. Immediately acquiring a second holder (succeeds since OS lock is free)
// 4. Unblocking the first cleanup goroutine
// 5. Verifying that the second holder's PID/generation are NOT clobbered
//
// REVERT CHECK PROCEDURE:
//  1. Temporarily disable the generation check in clearHolderMetadata by
//     commenting out the currentGen != expectedGeneration early return.
//  2. Run: go test ./internal/session -run TestP1_5_GenerationCheckPreventsMetadataClobber -v
//  3. The test will FAIL because the first cleanup clobbers the second owner's metadata.
//  4. Restore the generation check and the test will PASS.
func TestP1_5_GenerationCheckPreventsMetadataClobber(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-p1-5-clobber"

	var cleanupCompleted atomic.Bool

	// Acquire the first lock with a slow cleanup function.
	lk1, err := TryAcquireSessionLockWithOptions(tmpDir, sessionID,
		WithClearHolderMetadataFn(func(path string, expectedGeneration string) {
			// Block longer than releaseMetadataCleanupBound (50ms) to
			// ensure Release() returns before cleanup runs.
			time.Sleep(100 * time.Millisecond)
			clearHolderMetadata(path, expectedGeneration)
			cleanupCompleted.Store(true)
		}))
	require.NoError(t, err, "first TryAcquireSessionLock should succeed")
	require.NotNil(t, lk1)

	// Store first holder's generation for later verification.
	firstGeneration := lk1.generation

	// Release the first lock. This should return quickly (within ~50ms)
	// even though cleanup is blocked for 100ms.
	releaseStart := time.Now()
	releaseErr := lk1.Release()
	releaseDuration := time.Since(releaseStart)

	require.NoError(t, releaseErr, "first Release should succeed")
	require.Less(t, releaseDuration, 100*time.Millisecond,
		"first Release should return within 100ms despite slow cleanup")

	// Immediately acquire the lock again as a second owner.
	// This should succeed because the OS lock is already free.
	lk2, err := TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err, "second TryAcquireSessionLock should succeed immediately")
	require.NotNil(t, lk2)

	// Store second holder's metadata.
	secondGeneration := lk2.generation
	secondPID := lk2.HolderPID
	sidecarPath2 := pidSidecarPath(lk2.Path)
	genPath2 := generationSidecarPath(lk2.Path)

	// Verify the two owners have different generations (critical invariant).
	require.NotEqual(t, firstGeneration, secondGeneration,
		"different acquire instances must have different generations")

	// Wait for the first cleanup goroutine to complete.
	require.Eventually(t, func() bool {
		return cleanupCompleted.Load()
	}, 2*time.Second, 10*time.Millisecond, "first cleanup should complete within 2s")

	// CRITICAL ASSERTION: The second owner's metadata should NOT be clobbered.
	// Read the files directly from disk to verify what's actually there.

	// Verify the lock file contains the second owner's PID.
	pidFromLock := ReadLockPID(lk2.Path)
	require.Equal(t, secondPID, pidFromLock,
		"lock file should still contain second owner's PID after first cleanup completes")

	// Verify the PID sidecar contains the second owner's PID.
	sidecarBytes, err := os.ReadFile(sidecarPath2)
	require.NoError(t, err, "PID sidecar should exist after second acquire")
	sidecarContent := string(sidecarBytes)
	require.Contains(t, sidecarContent, string(rune('0'+secondPID%10)),
		"PID sidecar should contain second owner's PID")

	// Verify the generation sidecar contains the second owner's generation.
	genBytes, err := os.ReadFile(genPath2)
	require.NoError(t, err, "generation sidecar should exist after second acquire")
	currentGen := string(genBytes)
	require.Equal(t, secondGeneration, currentGen,
		"generation sidecar should contain second owner's generation")

	// Verify the first owner's generation is NOT present (it was correctly skipped).
	require.NotEqual(t, firstGeneration, currentGen,
		"generation sidecar should NOT contain first owner's generation")

	// Cleanup the second lock.
	require.NoError(t, lk2.Release(), "second Release should succeed")

	// Wait for second cleanup to complete.
	require.Eventually(t, func() bool {
		pid := ReadLockPID(lk2.Path)
		return pid == 0
	}, 2*time.Second, 10*time.Millisecond, "second cleanup should clear PID")
}

// TestP1_5_GenerationCheckBackwardCompatible verifies that clearHolderMetadata
// falls back to the pre-fix unconditional cleanup behavior when the
// generation sidecar is missing (an old-format lock file predating the
// generation mechanism, or this holder's own writeGenerationSidecar call
// having failed at acquire time — best-effort, never fatal).
//
// SCOPE NOTE (added on independent review): the delegated /crush fix's own
// version of this test asserted the OPPOSITE — that cleanup is SKIPPED
// when the generation sidecar is missing — treating "file absent" as
// positive evidence of a new owner. That is wrong: a missing sidecar is
// exactly as consistent with "this holder's own best-effort sidecar write
// failed" as with "a new owner is present", and skipping unconditionally
// turns an occasional, harmless write hiccup into a PID that never clears
// on release again — worse than the narrow clobber window P1-5 exists to
// close, and a regression from the pre-fix unconditional-cleanup
// behavior. Fixed clearHolderMetadata to only skip on a POSITIVE
// mismatch (file exists AND contains a different generation); a missing
// or unreadable sidecar now falls back to cleaning up, exactly as before
// this fix existed.
func TestP1_5_GenerationCheckBackwardCompatible(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-p1-5-backward-compat"

	// Create a lock file the old way (without generation sidecar).
	locksDir := filepath.Join(tmpDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-"+sanitiseSessionID(sessionID)+".lock")

	// Write a simple PID-only lock file (old format).
	require.NoError(t, os.WriteFile(lockPath, []byte("12345\n"), 0o644))
	require.NoError(t, os.WriteFile(pidSidecarPath(lockPath), []byte("12345\n"), 0o644))

	// clearHolderMetadata must still clean up when the generation sidecar is
	// missing: absence is not positive evidence of a new owner.
	clearHolderMetadata(lockPath, "some-expected-generation")

	// The PID must have been cleared (cleanup proceeded, not skipped).
	pidFromLock := ReadLockPID(lockPath)
	require.Equal(t, 0, pidFromLock,
		"PID must be cleared when the generation sidecar is missing — absence is not evidence of a new owner")

	// Cleanup manually (lock file itself is left in place by design; only
	// content is cleared — see clearHolderMetadata's own doc).
	_ = os.Remove(lockPath)
	_ = os.Remove(pidSidecarPath(lockPath))
}

// TestP1_5_GenerationCheckSkipsOnPositiveMismatch verifies the actual
// clobber-prevention behavior: clearHolderMetadata skips cleanup ONLY when
// the generation sidecar exists and contains a DIFFERENT generation than
// expected (real evidence of a new owner), as opposed to merely being
// absent (see TestP1_5_GenerationCheckBackwardCompatible for that case).
func TestP1_5_GenerationCheckSkipsOnPositiveMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-p1-5-positive-mismatch"

	locksDir := filepath.Join(tmpDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-"+sanitiseSessionID(sessionID)+".lock")

	require.NoError(t, os.WriteFile(lockPath, []byte("12345\n"), 0o644))
	require.NoError(t, os.WriteFile(pidSidecarPath(lockPath), []byte("12345\n"), 0o644))
	require.NoError(t, os.WriteFile(generationSidecarPath(lockPath), []byte("some-other-generation"), 0o644))

	// expectedGeneration does not match what's on disk — a real new owner's
	// generation. Cleanup must be skipped.
	clearHolderMetadata(lockPath, "stale-generation-from-old-holder")

	pidFromLock := ReadLockPID(lockPath)
	require.Equal(t, 12345, pidFromLock,
		"PID must be preserved when the on-disk generation genuinely differs from expected")

	genBytes, err := os.ReadFile(generationSidecarPath(lockPath))
	require.NoError(t, err)
	require.Equal(t, "some-other-generation", string(genBytes),
		"generation sidecar must be untouched when cleanup is correctly skipped")

	_ = os.Remove(lockPath)
	_ = os.Remove(pidSidecarPath(lockPath))
	_ = os.Remove(generationSidecarPath(lockPath))
}

// TestP1_5_GenerationMultipleSequentialReleases verifies that sequential
// acquire/release cycles correctly update the generation each time and
// that stale cleanup goroutines never clobber a newer owner's metadata.
func TestP1_5_GenerationMultipleSequentialReleases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sessionID := "test-session-p1-5-sequential"

	var generations []string

	// Acquire and release the lock 5 times sequentially.
	for i := 0; i < 5; i++ {
		lk, err := TryAcquireSessionLock(tmpDir, sessionID)
		require.NoError(t, err, "acquire %d should succeed", i)
		require.NotNil(t, lk)

		generations = append(generations, lk.generation)

		// Verify each generation is unique.
		for j := 0; j < i; j++ {
			require.NotEqual(t, generations[j], generations[i],
				"generation %d should differ from generation %d", j, i)
		}

		// Release and wait for cleanup to complete before next cycle.
		require.NoError(t, lk.Release(), "release %d should succeed", i)

		require.Eventually(t, func() bool {
			pid := ReadLockPID(lk.Path)
			return pid == 0
		}, 2*time.Second, 10*time.Millisecond, "cleanup %d should complete", i)
	}

	// Verify we have 5 distinct generations.
	require.Len(t, generations, 5, "should have 5 distinct generations")
}
