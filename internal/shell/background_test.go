package shell

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackgroundShellManager_Start(t *testing.T) {
	t.Skip("Skipping this until I figure out why its flaky")
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'hello world'", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	if bgShell.ID == "" {
		t.Error("expected shell ID to be non-empty")
	}

	// Wait for the command to complete
	bgShell.Wait()

	stdout, stderr, done, err := bgShell.GetOutput()
	if !done {
		t.Error("expected shell to be done")
	}

	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	if !strings.Contains(stdout, "hello world") {
		t.Errorf("expected stdout to contain 'hello world', got: %s", stdout)
	}

	if stderr != "" {
		t.Errorf("expected empty stderr, got: %s", stderr)
	}
}

func TestBackgroundShellManager_Get(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'test'", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Retrieve the shell
	retrieved, ok := manager.Get(bgShell.ID)
	if !ok {
		t.Error("expected to find the background shell")
	}

	if retrieved.ID != bgShell.ID {
		t.Errorf("expected shell ID %s, got %s", bgShell.ID, retrieved.ID)
	}

	// Clean up
	_ = manager.Kill(t.Context(), bgShell.ID)
}

func TestBackgroundShellManager_Kill(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start a long-running command
	bgShell, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Kill it
	err = manager.Kill(t.Context(), bgShell.ID)
	if err != nil {
		t.Errorf("failed to kill background shell: %v", err)
	}

	// Verify it's no longer in the manager
	_, ok := manager.Get(bgShell.ID)
	if ok {
		t.Error("expected shell to be removed after kill")
	}

	// Verify the shell is done
	if !bgShell.IsDone() {
		t.Error("expected shell to be done after kill")
	}
}

func TestBackgroundShellManager_KillNonExistent(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()

	err := manager.Kill(t.Context(), "non-existent-id")
	if err == nil {
		t.Error("expected error when killing non-existent shell")
	}
}

// TestBackgroundShellManager_Kill_ReturnsOnCtxDone proves Kill honours its
// context argument and returns promptly when the underlying process ignores
// cancellation, instead of blocking forever on <-shell.done. The OLD Kill
// signature took no context and unconditionally blocked on <-shell.done, so
// under this exact scenario it would hang forever — this test fails (times
// out) against the old code and passes against the new code.
//
// Rather than depend on OS-specific signal-trapping behaviour (which the
// sibling KillAll_Timeout test does, and which is unreliable on Windows where
// mvdan/sh kills the subprocess eagerly on context cancel), we inject a
// synthetic BackgroundShell whose done channel NEVER closes and whose cancel
// func is a no-op. This deterministically reproduces the real-world failure
// mode the fix targets: cancel() was called but the process didn't exit.
//
// We can't use synctest here for the same reason KillAll_Timeout can't: it
// trips -race.
func TestBackgroundShellManager_Kill_ReturnsOnCtxDone(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()

	// Synthetic shell: cancel is a no-op and done never closes, modelling a
	// stuck child process tree that ignores context cancellation (the exact
	// scenario observed with rsbuild dev / node.exe on Windows).
	bgShell := &BackgroundShell{
		ID:        "STUCK",
		done:      make(chan struct{}), // never closed
		cancel:    func() {},           // no-op: cancellation does nothing
		ctx:       context.Background(),
		stdout:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		stderr:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		StartTime: time.Now(),
	}
	manager.shells.Set(bgShell.ID, bgShell)

	// Already-expired context so Kill must give up immediately on the
	// ctx.Done() branch of the select.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	err := manager.Kill(ctx, bgShell.ID)
	elapsed := time.Since(start)

	// Must return the context error, not nil and not hang.
	require.ErrorIs(t, err, context.Canceled)
	// Must return promptly — the old code would still be blocked here.
	require.Less(t, elapsed, 2*time.Second)

	// Shell must have been removed from the manager regardless.
	_, ok := manager.Get(bgShell.ID)
	require.False(t, ok, "shell should be removed from manager even when Kill gives up waiting")
}

func TestBackgroundShell_IsDone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'quick'", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Wait for the command to complete (Windows is slower to spin up).
	require.Eventually(t, bgShell.IsDone, 5*time.Second, 50*time.Millisecond, "expected shell to be done")

	// Clean up
	_ = manager.Kill(t.Context(), bgShell.ID)
}

func TestBackgroundShell_WithBlockFuncs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	blockFuncs := []BlockFunc{
		CommandsBlocker([]string{"curl", "wget"}),
	}

	bgShell, err := manager.Start(ctx, workingDir, blockFuncs, "curl example.com", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Wait for the command to complete
	bgShell.Wait()

	stdout, stderr, done, execErr := bgShell.GetOutput()
	if !done {
		t.Error("expected shell to be done")
	}

	// The command should have been blocked
	output := stdout + stderr
	if !strings.Contains(output, "not allowed") && execErr == nil {
		t.Errorf("expected command to be blocked, got stdout: %s, stderr: %s, err: %v", stdout, stderr, execErr)
	}

	// Clean up
	_ = manager.Kill(t.Context(), bgShell.ID)
}

func TestBackgroundShellManager_List(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping flacky test on windows")
	}

	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start two shells
	bgShell1, err := manager.Start(ctx, workingDir, nil, "sleep 1", "")
	if err != nil {
		t.Fatalf("failed to start first background shell: %v", err)
	}

	bgShell2, err := manager.Start(ctx, workingDir, nil, "sleep 1", "")
	if err != nil {
		t.Fatalf("failed to start second background shell: %v", err)
	}

	ids := manager.List()

	// Check that both shells are in the list
	found1 := false
	found2 := false
	for _, id := range ids {
		if id == bgShell1.ID {
			found1 = true
		}
		if id == bgShell2.ID {
			found2 = true
		}
	}

	if !found1 {
		t.Errorf("expected to find shell %s in list", bgShell1.ID)
	}
	if !found2 {
		t.Errorf("expected to find shell %s in list", bgShell2.ID)
	}

	// Clean up
	_ = manager.Kill(t.Context(), bgShell1.ID)
	_ = manager.Kill(t.Context(), bgShell2.ID)
}

func TestBackgroundShellManager_KillAll(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start multiple long-running shells
	shell1, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start shell 1: %v", err)
	}

	shell2, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start shell 2: %v", err)
	}

	shell3, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start shell 3: %v", err)
	}

	// Verify shells are running
	if shell1.IsDone() || shell2.IsDone() || shell3.IsDone() {
		t.Error("shells should not be done yet")
	}

	// Kill all shells
	manager.KillAll(t.Context())

	// Verify all shells are done
	if !shell1.IsDone() {
		t.Error("shell1 should be done after KillAll")
	}
	if !shell2.IsDone() {
		t.Error("shell2 should be done after KillAll")
	}
	if !shell3.IsDone() {
		t.Error("shell3 should be done after KillAll")
	}

	// Verify they're removed from the manager
	if _, ok := manager.Get(shell1.ID); ok {
		t.Error("shell1 should be removed from manager")
	}
	if _, ok := manager.Get(shell2.ID); ok {
		t.Error("shell2 should be removed from manager")
	}
	if _, ok := manager.Get(shell3.ID); ok {
		t.Error("shell3 should be removed from manager")
	}

	// Verify list is empty (or doesn't contain our shells)
	ids := manager.List()
	for _, id := range ids {
		if id == shell1.ID || id == shell2.ID || id == shell3.ID {
			t.Errorf("shell %s should not be in list after KillAll", id)
		}
	}
}

func TestBackgroundShellManager_KillAll_Timeout(t *testing.T) {
	t.Parallel()

	// XXX: can't use synctest here - causes --race to trip.

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start a shell that traps signals and ignores cancellation.
	_, err := manager.Start(t.Context(), workingDir, nil, "trap '' TERM INT; sleep 60", "")
	require.NoError(t, err)

	// Short timeout to test the timeout path.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	t.Cleanup(cancel)

	start := time.Now()
	manager.KillAll(ctx)

	elapsed := time.Since(start)

	// Must return promptly after timeout, not hang for 60 seconds.
	require.Less(t, elapsed, 2*time.Second)
}

func TestBackgroundShell_WaitContext_Completed(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	close(done)

	bgShell := &BackgroundShell{done: done}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)

	require.True(t, bgShell.WaitContext(ctx))
}

func TestBackgroundShell_WaitContext_Canceled(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{done: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.False(t, bgShell.WaitContext(ctx))
}

// TestBackgroundShell_WaitForChange_ReturnsOnOutputGrowth proves the wait
// returns promptly when buffered output grows past the supplied baseline,
// without waiting for the job to finish or the ctx to time out.
func TestBackgroundShell_WaitForChange_ReturnsOnOutputGrowth(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		stderr:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	// Baseline is one byte; we will write more into the buffer.
	sinceLen := 1

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		bgShell.WaitForChange(ctx, sinceLen)
		close(done)
	}()

	// Give the goroutine a moment to enter the select, then push output.
	time.Sleep(350 * time.Millisecond)
	bgShell.stdout.WriteString("hello world")

	select {
	case <-done:
		// Expected: returned because output grew past baseline.
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForChange did not return after output grew past baseline")
	}
}

// TestBackgroundShell_WaitForChange_ReturnsOnCompletion proves the wait
// returns the moment the job's done channel closes.
func TestBackgroundShell_WaitForChange_ReturnsOnCompletion(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		stderr:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		// Baseline huge so the only way out is the done channel.
		bgShell.WaitForChange(ctx, 1<<30)
		close(done)
	}()

	time.Sleep(350 * time.Millisecond)
	close(bgShell.done)

	select {
	case <-done:
		// Expected: returned because job completed.
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForChange did not return after job completed")
	}
}

// TestBackgroundShell_WaitForChange_ReturnsOnCtxDone proves the wait never
// blocks indefinitely: a ctx that times out (or is canceled) without any
// output growth or job completion still unblocks the caller.
func TestBackgroundShell_WaitForChange_ReturnsOnCtxDone(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		stderr:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	t.Cleanup(cancel)

	start := time.Now()
	// Huge baseline, never-completing job → only ctx.Done() can fire.
	bgShell.WaitForChange(ctx, 1<<30)
	elapsed := time.Since(start)

	// Must return ~right after the ctx deadline, not hang.
	require.Less(t, elapsed, 2*time.Second, "WaitForChange should return when ctx ends")
}

// TestBackgroundShell_OnDone_FiresOnCompletion proves OnDone fires promptly
// when a short-lived background command finishes on its own.
func TestBackgroundShell_OnDone_FiresOnCompletion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo hi", "")
	require.NoError(t, err)

	fired := make(chan struct{})
	bgShell.OnDone(func() { close(fired) })

	select {
	case <-fired:
		// Expected: OnDone fired once the echo finished.
	case <-time.After(3 * time.Second):
		t.Fatal("OnDone did not fire after command completed")
	}

	// Clean up (no-op once already gone, but keeps the manager tidy).
	_ = manager.Kill(t.Context(), bgShell.ID)
}

// TestBackgroundShell_OnDone_FiresOnKill proves OnDone does NOT fire while a
// long-running command is still alive, but DOES fire promptly once the job is
// killed via the manager.
func TestBackgroundShell_OnDone_FiresOnKill(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "sleep 30", "")
	require.NoError(t, err)

	fired := make(chan struct{})
	bgShell.OnDone(func() { close(fired) })

	// While the job is alive, OnDone must not fire.
	select {
	case <-fired:
		t.Fatal("OnDone fired while command was still running")
	case <-time.After(300 * time.Millisecond):
		// Expected: still running.
	}

	// Killing the job must release OnDone.
	require.NoError(t, manager.Kill(t.Context(), bgShell.ID))

	select {
	case <-fired:
		// Expected: OnDone fired after Kill.
	case <-time.After(3 * time.Second):
		t.Fatal("OnDone did not fire after Kill")
	}
}

// TestBackgroundShell_Elapsed confirms Elapsed reports a non-zero duration
// once StartTime is set, and zero when it is not.
func TestBackgroundShell_Elapsed(t *testing.T) {
	t.Parallel()

	t.Run("zero when unset", func(t *testing.T) {
		t.Parallel()
		bgShell := &BackgroundShell{}
		require.Zero(t, bgShell.Elapsed())
	})

	t.Run("positive when set", func(t *testing.T) {
		t.Parallel()
		bgShell := &BackgroundShell{StartTime: time.Now()}
		time.Sleep(5 * time.Millisecond)
		require.Positive(t, bgShell.Elapsed())
	})
}

// TestBoundedBuffer_CapsSnapshotSize proves that writing far more than
// maxBytes into a boundedBuffer never yields a String() snapshot larger than
// the configured cap (plus the small, fixed marker overhead already
// accounted for in enforceLimitLocked's budget) — this is the core memory
// fix: previously (syncBuffer wrapping bytes.Buffer) an arbitrarily large
// write grew the snapshot without bound.
func TestBoundedBuffer_CapsSnapshotSize(t *testing.T) {
	t.Parallel()

	const maxBytes = 64 * 1024 // small cap so the test writes fast
	b := newBoundedBuffer(maxBytes)

	line := strings.Repeat("x", 100) + "\n"
	totalWritten := 0
	// Write ~50x the cap worth of data.
	for totalWritten < maxBytes*50 {
		n, err := b.WriteString(line)
		require.NoError(t, err)
		totalWritten += n
	}

	snapshot := b.String()
	require.LessOrEqual(t, len(snapshot), maxBytes,
		"bounded buffer snapshot must never exceed the configured cap")

	// The monotonic counter must reflect everything ever written, not just
	// what's resident.
	require.Equal(t, totalWritten, b.Len())
	require.Greater(t, b.Len(), maxBytes, "test sanity: must have written more than the cap")
}

// TestBoundedBuffer_PreservesHeadAndTailWithMarker proves that once a
// boundedBuffer overflows, the snapshot contains BOTH the first bytes ever
// written (head — usually identifies the command / earliest errors) AND the
// most recently written bytes (tail — current state), joined by an explicit
// truncation marker reporting how many bytes were dropped from the middle.
// This is the classic head+tail log-truncation pattern; a naive "keep only
// the first N" or "keep only the last N" (ring buffer) policy would fail
// this test by construction.
func TestBoundedBuffer_PreservesHeadAndTailWithMarker(t *testing.T) {
	t.Parallel()

	const maxBytes = 8 * 1024
	b := newBoundedBuffer(maxBytes)

	require.NoError(t, mustWrite(b, "HEAD-MARKER-START\n"))

	// Write enough padding to guarantee the head marker would be evicted by
	// a plain ring-buffer / tail-only policy.
	padding := strings.Repeat("pad-line-filler-content\n", 2000)
	require.NoError(t, mustWrite(b, padding))

	require.NoError(t, mustWrite(b, "TAIL-MARKER-END\n"))

	snapshot := b.String()

	require.Contains(t, snapshot, "HEAD-MARKER-START", "head must survive truncation")
	require.Contains(t, snapshot, "TAIL-MARKER-END", "tail must survive truncation")
	require.Regexp(t, `\[\d+ bytes truncated\]`, snapshot, "must report an explicit truncation marker with a byte count")
	require.LessOrEqual(t, len(snapshot), maxBytes)
}

func mustWrite(b *boundedBuffer, s string) error {
	_, err := b.WriteString(s)
	return err
}

// TestBackgroundShell_WaitForChange_DetectsGrowthAfterOverflow proves
// WaitForChange keeps detecting new output as "change" even once a stream's
// bounded buffer has already overflowed and started dropping old bytes —
// i.e. the monotonic writtenBytes counter (not the bounded/possibly-shrunk
// resident snapshot) drives the comparison, so growth is never silently
// missed after truncation kicks in.
func TestBackgroundShell_WaitForChange_DetectsGrowthAfterOverflow(t *testing.T) {
	t.Parallel()

	const maxBytes = 4 * 1024
	bgShell := &BackgroundShell{
		stdout:    newBoundedBuffer(maxBytes),
		stderr:    newBoundedBuffer(maxBytes),
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	// Overflow the buffer well past its cap before establishing a baseline.
	overflow := strings.Repeat("overflow-line-content-here\n", 1000)
	_, err := bgShell.stdout.WriteString(overflow)
	require.NoError(t, err)
	require.Greater(t, bgShell.stdout.Len(), maxBytes, "test sanity: must have overflowed")

	baseline := bgShell.stdout.Len() + bgShell.stderr.Len()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)

	waitDone := make(chan struct{})
	go func() {
		bgShell.WaitForChange(ctx, baseline)
		close(waitDone)
	}()

	// Confirm it does NOT return immediately (no growth yet).
	select {
	case <-waitDone:
		t.Fatal("WaitForChange returned before any new output was written past the baseline")
	case <-time.After(300 * time.Millisecond):
		// Expected: still waiting.
	}

	// Write more — even though this keeps overflowing the resident buffer,
	// the monotonic counter must still grow and WaitForChange must notice.
	_, err = bgShell.stdout.WriteString("more-output-after-overflow\n")
	require.NoError(t, err)

	select {
	case <-waitDone:
		// Expected: detected growth past baseline despite ongoing truncation.
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForChange did not detect growth after buffer overflow")
	}
}

// TestBackgroundShell_ConcurrentReadWrite exercises concurrent GetOutput /
// WaitForChange readers against a concurrent writer goroutine (mirroring the
// real ExecStream call pattern where the shell interpreter writes
// continuously while job_output/bash poll concurrently). Intended to run
// under `-race`: it must complete without triggering the race detector and
// without ever observing a snapshot larger than the cap.
func TestBackgroundShell_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()

	const maxBytes = 16 * 1024
	bgShell := &BackgroundShell{
		stdout:    newBoundedBuffer(maxBytes),
		stderr:    newBoundedBuffer(maxBytes),
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	var wg sync.WaitGroup

	// Writer: simulates ExecStream writing continuously.
	wg.Go(func() {
		for i := 0; i < 2000; i++ {
			_, _ = bgShell.stdout.WriteString("line of output data\n")
			_, _ = bgShell.stderr.WriteString("err line\n")
		}
	})

	// Readers: simulate job_output polling via GetOutput and WaitForChange.
	for range 4 {
		wg.Go(func() {
			for i := 0; i < 200; i++ {
				stdout, stderr, _, _ := bgShell.GetOutput()
				require.LessOrEqual(t, len(stdout), maxBytes)
				require.LessOrEqual(t, len(stderr), maxBytes)
			}
		})
	}
	wg.Go(func() {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		bgShell.WaitForChange(ctx, 0)
	})

	wg.Wait()
	close(bgShell.done)
}

// TestBackgroundShell_ReleaseBuffers_KeepsPlaceholderNotEmpty proves that
// once buffers are released post-completion, GetOutput doesn't silently
// return an empty string for a stream that actually produced output — that
// would look indistinguishable from "the command produced no output", which
// is a regression in its own right. It should instead surface an explicit
// placeholder.
func TestBackgroundShell_ReleaseBuffers_KeepsPlaceholderNotEmpty(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout: newBoundedBuffer(maxStreamBufferBytes),
		stderr: newBoundedBuffer(maxStreamBufferBytes),
		done:   make(chan struct{}),
	}
	_, err := bgShell.stdout.WriteString("some real output")
	require.NoError(t, err)
	close(bgShell.done)

	stdoutBefore, _, _, _ := bgShell.GetOutput()
	require.Equal(t, "some real output", stdoutBefore)

	bgShell.releaseBuffers()

	stdoutAfter, stderrAfter, done, _ := bgShell.GetOutput()
	require.True(t, done)
	require.NotEmpty(t, stdoutAfter, "must not silently look like empty output after release")
	require.NotEqual(t, "some real output", stdoutAfter, "test sanity: content must actually have been released")
	require.Empty(t, stderrAfter, "stream that never produced output stays empty after release")
}

// TestBackgroundShellManager_Start_AtomicLimitCheck proves the
// check-then-insert sequence in Start (Len() >= MaxBackgroundJobs, then
// Set()) can't be overshot by concurrent callers racing past the check
// before either has inserted. Runs many concurrent Start calls against a
// manager whose limit is effectively the real MaxBackgroundJobs constant, and
// asserts the resulting count never exceeds it.
func TestBackgroundShellManager_Start_AtomicLimitCheck(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	const attempts = MaxBackgroundJobs + 20
	var wg sync.WaitGroup
	var succeeded atomic.Int64
	for range attempts {
		wg.Go(func() {
			// Use a long-running command so started jobs STAY active for the
			// whole test. The limit is enforced on ACTIVE (not total/retained)
			// jobs: if any completed mid-test they would legitimately free
			// their slot, and the "never overshoot" invariant could no longer
			// be expressed as succeeded <= MaxBackgroundJobs.
			_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
			if err == nil {
				succeeded.Add(1)
			}
		})
	}
	wg.Wait()

	require.LessOrEqual(t, int(succeeded.Load()), MaxBackgroundJobs,
		"concurrent Start calls must never overshoot MaxBackgroundJobs")
	require.Equal(t, manager.ActiveJobs(), int(succeeded.Load()),
		"all retained jobs are still active here, so active count must equal succeeded")
	require.LessOrEqual(t, manager.shells.Len(), MaxBackgroundJobs)

	// Clean up any still-tracked shells.
	manager.KillAll(t.Context())
}

// TestBackgroundShell_TotalWrittenBytes_SurvivesOverflow proves
// TotalWrittenBytes (the correct baseline for WaitForChange) keeps growing
// 1:1 with real writes even once the underlying stream has overflowed its
// cap and GetOutput's snapshot has stopped growing at the same rate. This
// guards against a regression where a caller mistakenly derives its
// WaitForChange baseline from len(stdout)+len(stderr) (a bounded snapshot)
// instead of TotalWrittenBytes: once overflowed, such a baseline would
// already sit below the live monotonic counters, making WaitForChange return
// immediately (falsely reporting "new output") instead of actually waiting.
func TestBackgroundShell_TotalWrittenBytes_SurvivesOverflow(t *testing.T) {
	t.Parallel()

	const maxBytes = 2 * 1024
	bgShell := &BackgroundShell{
		stdout:    newBoundedBuffer(maxBytes),
		stderr:    newBoundedBuffer(maxBytes),
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	// Overflow stdout well past its cap.
	overflow := strings.Repeat("line-of-output-content\n", 500)
	_, err := bgShell.stdout.WriteString(overflow)
	require.NoError(t, err)

	snapshot := bgShell.stdout.String()
	total := bgShell.TotalWrittenBytes()

	require.Greater(t, total, len(snapshot),
		"once overflowed, the monotonic total must exceed the bounded snapshot length")
	require.Equal(t, len(overflow), total, "total must equal every byte ever written, not just what's resident")

	// A baseline taken from TotalWrittenBytes right now must NOT look like
	// "growth" has already happened relative to itself.
	baseline := bgShell.TotalWrittenBytes()
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	t.Cleanup(cancel)

	start := time.Now()
	bgShell.WaitForChange(ctx, baseline)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, 350*time.Millisecond,
		"WaitForChange must actually wait out the ctx deadline when using a TotalWrittenBytes baseline with no further writes, not return immediately")
}

// TestBoundedBuffer_Release_FreesBackingMemory proves release() actually
// drops the backing arrays (letting GC reclaim them), not just resets the
// logical length — bytes.Buffer.Reset() keeps the capacity (documented stdlib
// behaviour), which would leave up to maxStreamBufferBytes per stream resident
// in the heap indefinitely. We check cap() of the underlying slice directly:
// after release it must be zero.
func TestBoundedBuffer_Release_FreesBackingMemory(t *testing.T) {
	t.Parallel()

	b := newBoundedBuffer(maxStreamBufferBytes)

	// Write well past the cap to force both a large backing-array allocation
	// and trigger truncation (so all counter fields are non-zero).
	big := strings.Repeat("x", maxStreamBufferBytes*2) // 6 MiB
	_, err := b.WriteString(big)
	require.NoError(t, err)

	// Capture counter state before release.
	writtenBefore := b.writtenBytes.Load()
	truncatedBefore := b.truncated.Load()
	droppedBefore := b.droppedBytes.Load()
	require.Positive(t, writtenBefore, "sanity: must have written data")
	require.True(t, truncatedBefore, "sanity: must have triggered truncation")
	require.Positive(t, droppedBefore, "sanity: must have dropped bytes")

	require.Positive(t, cap(b.buf.Bytes()),
		"sanity: buf must have a backing array")

	b.release()

	// Direct memory-free assertion: cap must be zero after release.
	// Reset() would have kept the old capacity alive here.
	require.Zero(t, cap(b.buf.Bytes()),
		"release must drop the buf backing array, not just reset the length")
	require.Zero(t, cap(b.head.Bytes()),
		"release must drop the head backing array, not just reset the length")

	// Counters must be preserved — they drive TotalWrittenBytes /
	// WaitForChange baseline and String()'s truncation marker.
	require.Equal(t, writtenBefore, b.writtenBytes.Load(),
		"writtenBytes must survive release")
	require.Equal(t, truncatedBefore, b.truncated.Load(),
		"truncated flag must survive release")
	require.Equal(t, droppedBefore, b.droppedBytes.Load(),
		"droppedBytes must survive release")
}

// TestBackgroundShell_BufferReleaseTimer_FiresWithoutCleanup proves the
// post-completion buffer-release timer (armed in Start's completion goroutine)
// fires and releases buffers after the retention window WITHOUT requiring a
// subsequent bash task to trigger Cleanup. This is the core scheduling fix:
// previously releaseBuffers was only reachable via Cleanup, which only ran
// when the next bash task started.
//
// NOT parallel: temporarily overrides the package-level bufferRetention.
func TestBackgroundShell_BufferReleaseTimer_FiresWithoutCleanup(t *testing.T) {
	// Override the retention to a short window so the test runs fast.
	originalRetention := bufferRetention
	bufferRetention = 100 * time.Millisecond
	t.Cleanup(func() { bufferRetention = originalRetention })

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo hi", "")
	require.NoError(t, err)

	// Wait for the job to complete.
	require.Eventually(t, bgShell.IsDone, 5*time.Second, 50*time.Millisecond,
		"job should complete")

	// Sanity: buffers must NOT be released immediately after completion —
	// the timer hasn't fired yet.
	require.False(t, bgShell.bufReleased.Load(),
		"buffers must not be released immediately after completion")

	// Now wait for the timer to fire — well past the 100ms retention but far
	// less than the default 15 minutes. Crucially, we do NOT call Cleanup
	// or start any new bash task: the timer is the sole release trigger.
	require.Eventually(t, func() bool {
		return bgShell.bufReleased.Load()
	}, 3*time.Second, 20*time.Millisecond,
		"buffer-release timer must fire without a Cleanup call or new bash task")

	// TotalWrittenBytes must still report the real total after release
	// (writtenBytes is preserved, not zeroed) — this guards the
	// WaitForChange baseline path used by job_output.
	require.Positive(t, bgShell.TotalWrittenBytes(),
		"TotalWrittenBytes must survive timer-based buffer release")
}

// TestBackgroundShell_ReleaseBuffers_Idempotent proves calling releaseBuffers
// multiple times — e.g. the completion timer fires AND then Cleanup runs when
// the next bash task starts — is safe: no panic, no state corruption. The
// bufReleased Swap guard inside releaseBuffers ensures only the first call
// performs the actual release.
func TestBackgroundShell_ReleaseBuffers_Idempotent(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout: newBoundedBuffer(maxStreamBufferBytes),
		stderr: newBoundedBuffer(maxStreamBufferBytes),
		done:   make(chan struct{}),
	}
	_, err := bgShell.stdout.WriteString("real output content")
	require.NoError(t, err)
	close(bgShell.done)

	totalBefore := bgShell.TotalWrittenBytes()
	require.Positive(t, totalBefore)

	// First release (simulates the completion timer firing).
	require.NotPanics(t, func() { bgShell.releaseBuffers() })
	require.True(t, bgShell.bufReleased.Load(),
		"bufReleased must be set after first release")

	stdoutAfter1, _, done1, _ := bgShell.GetOutput()
	require.True(t, done1)
	require.NotEmpty(t, stdoutAfter1,
		"placeholder must be returned after first release")

	// TotalWrittenBytes must survive the first release.
	require.Equal(t, totalBefore, bgShell.TotalWrittenBytes(),
		"TotalWrittenBytes must not change after release")

	// Second release (simulates Cleanup running later). Must not panic.
	require.NotPanics(t, func() { bgShell.releaseBuffers() })

	// Observable state must be identical after the second call.
	stdoutAfter2, _, done2, _ := bgShell.GetOutput()
	require.True(t, done2)
	require.Equal(t, stdoutAfter1, stdoutAfter2,
		"second releaseBuffers must not change observable state")
	require.Equal(t, totalBefore, bgShell.TotalWrittenBytes(),
		"TotalWrittenBytes must still be unchanged after double release")
}

// TestBackgroundShellManager_LimitIgnoresCompletedJobs proves the
// MaxBackgroundJobs limit counts only ACTIVE jobs: starting MORE than
// MaxBackgroundJobs jobs SEQUENTIALLY — each completing before the next starts
// — must succeed for every one of them, because finished jobs free their
// concurrency slot immediately. On the OLD Len()-based limit this test fails
// at the 51st Start: the 50 finished (but retained) jobs kept shells.Len() at
// 50, so the 51st was rejected even though nothing was running.
func TestBackgroundShellManager_LimitIgnoresCompletedJobs(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	const n = MaxBackgroundJobs + 5 // 55: comfortably past the limit
	for i := range n {
		bg, err := manager.Start(t.Context(), workingDir, nil, "echo hi", "")
		require.NoError(t, err,
			"job %d must start even though %d completed jobs are retained", i, i)
		// Wait for THIS job to finish before starting the next, so at most one
		// is active at a time and its slot is freed before the next Start.
		require.Eventually(t, bg.IsDone, 5*time.Second, 20*time.Millisecond,
			"job %d should complete so its slot is freed", i)
	}

	// No job is active after the loop; the retained (completed) entries are
	// still in the map, which is exactly the regression condition the old
	// Len()-based check choked on.
	require.Zero(t, manager.ActiveJobs(),
		"no jobs should be active after all completed")
	require.Equal(t, n, manager.shells.Len(),
		"completed jobs are retained in the map for querying")
}

// TestBackgroundShellManager_LimitBlocksWhenAllActive proves the limit still
// holds for genuinely concurrent jobs: with MaxBackgroundJobs long-running jobs
// all active at once, the next Start must be rejected. The active-counter fix
// must not weaken the real concurrency cap.
func TestBackgroundShellManager_LimitBlocksWhenAllActive(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	for range MaxBackgroundJobs {
		_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
		require.NoError(t, err)
	}
	require.Equal(t, MaxBackgroundJobs, manager.ActiveJobs(),
		"all started jobs are active")

	// The (MaxBackgroundJobs+1)th job must be rejected.
	_, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "maximum number of background jobs")

	// Clean up the long-running jobs.
	manager.KillAll(t.Context())
	require.Zero(t, manager.ActiveJobs())
}
