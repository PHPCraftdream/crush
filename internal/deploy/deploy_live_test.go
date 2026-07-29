//go:build windows

package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestReplaceFile_LiveWindowsProcess is the integration test called for in
// docs/plans/2026-07-29-relaunch-from-cache.md §9.2: it reproduces, against
// a genuinely running process, the exact rename-aside sequence deploy.go's
// replaceFile now uses on Windows instead of killing live crush sessions.
//
// It builds a tiny helper binary (testdata/sleeper), launches it so its
// exe file is loaded and executing, then applies deploy.go's rename-aside
// steps directly to that live exe:
//  1. rename dst -> dst.old-<token>   (must succeed even though the
//     process is running — this is the manual-experiment finding from
//     the plan's §2)
//  2. write a new file under the freed dst name (also must succeed)
//  3. assert the ORIGINAL process is still alive (proves the running
//     image survived the swap, not just that the rename call returned
//     success)
//  4. confirm deleting the renamed-aside file while the process is still
//     alive fails — this is the exact restriction rename-aside works
//     around, and SweepRenameAsideLeftovers must swallow that failure
//  5. signal the process to exit, then confirm SweepRenameAsideLeftovers
//     can now remove the renamed-aside file
//
// Restricted to windows via the build tag: rename-aside exists
// specifically to work around a Windows restriction (can't delete a busy
// file); on Unix, replaceFile already takes the plain temp+rename path
// and this scenario doesn't arise.
func TestReplaceFile_LiveWindowsProcess(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "sleeper.exe")

	buildSleeper(t, dst)

	stopFile := filepath.Join(dir, "stop")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, dst, stopFile)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(stopFile, []byte("stop"), 0o644)
		_ = cmd.Wait()
	})

	// Wait for the "ready" line so we know the process has actually
	// loaded and is executing its image, not just forked.
	waitForReady(t, stdout)

	// --- Step 1: rename dst aside while the process is alive. ---
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	aside := RenameAsideName(dst, token)
	if err := os.Rename(dst, aside); err != nil {
		t.Fatalf("rename-aside while process is live should succeed (confirmed by hand on 2026-07-29), got: %v", err)
	}

	// --- Step 2: write a new file under the freed dst name. ---
	newContents := []byte("new-build-marker")
	if err := os.WriteFile(dst, newContents, 0o755); err != nil {
		t.Fatalf("writing new binary under freed dst name should succeed, got: %v", err)
	}

	// --- Step 3: the ORIGINAL process must still be alive. ---
	if cmd.ProcessState != nil {
		t.Fatalf("sleeper process exited early (state: %v) — rename-aside must not disturb a running process", cmd.ProcessState)
	}
	time.Sleep(200 * time.Millisecond)
	if cmd.ProcessState != nil {
		t.Fatalf("sleeper process exited after rename-aside (state: %v)", cmd.ProcessState)
	}

	// --- Step 4: deleting the renamed-aside file while the process is alive must fail. ---
	if err := os.Remove(aside); err == nil {
		t.Fatalf("expected deleting the rename-aside file to fail while the process is still alive (confirms the exact restriction rename-aside works around); it succeeded instead")
	}

	// SweepRenameAsideLeftovers must not error and must leave the busy
	// file behind.
	_ = SweepRenameAsideLeftovers(dst)
	if _, err := os.Stat(aside); err != nil {
		t.Fatalf("expected the still-busy rename-aside file to survive the sweep, but it's gone: %v", err)
	}

	// --- Step 5: signal exit, then confirm cleanup now succeeds. ---
	if err := os.WriteFile(stopFile, []byte("stop"), 0o644); err != nil {
		t.Fatalf("write stop file: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("sleeper did not exit cleanly: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sleeper did not exit within 10s of the stop signal")
	}

	// Now that the process has exited, the leftover file must be
	// removable via the sweep helper. Filesystems can be slow to release
	// a handle right after process exit, so retry briefly before failing.
	removed := SweepRenameAsideLeftovers(dst)
	if len(removed) != 1 || removed[0] != aside {
		time.Sleep(300 * time.Millisecond)
		_ = SweepRenameAsideLeftovers(dst)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatalf("expected rename-aside leftover to be removable after process exit, still present (stat err: %v)", err)
	}

	// The new build must be exactly what we wrote in step 2 — the swap
	// didn't get reverted or corrupted by any of the above.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read final dst: %v", err)
	}
	if string(got) != string(newContents) {
		t.Fatalf("dst contents changed unexpectedly: got %q, want %q", got, newContents)
	}
}

// buildSleeper compiles internal/deploy/testdata/sleeper straight to
// outPath using the same `go` toolchain running the test.
func buildSleeper(t *testing.T, outPath string) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test file's path")
	}
	pkgDir := filepath.Join(filepath.Dir(thisFile), "testdata", "sleeper")

	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", outPath, ".")
	cmd.Dir = pkgDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building testdata/sleeper failed: %v\n%s", err, out)
	}
}

// waitForReady blocks until the sleeper process writes its "ready" line
// (or fails/times out).
func waitForReady(t *testing.T, stdout interface{ Read([]byte) (int, error) }) {
	t.Helper()
	buf := make([]byte, 64)
	done := make(chan struct{})
	var n int
	var readErr error
	go func() {
		n, readErr = stdout.Read(buf)
		close(done)
	}()
	select {
	case <-done:
		if readErr != nil && n == 0 {
			t.Fatalf("reading sleeper stdout: %v", readErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for sleeper to report ready")
	}
}
