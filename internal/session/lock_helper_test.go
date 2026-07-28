package session

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This file implements a real second-process test harness for
// TryAcquireSessionLock's reclaim protocol. A single-process test cannot
// exercise the bug this package guards against: POSIX flock semantics
// let the SAME process re-acquire (or appear to re-acquire) its own
// lock in ways that don't reflect true inter-process contention, so any
// claim about "does a live holder's lock survive a stale-looking
// mtime" has to be proven against an actual second OS process holding
// the actual OS-level lock.
//
// Pattern: re-exec the test binary itself with a sentinel environment
// variable (the classic Go stdlib "TestHelperProcess" idiom used by
// os/exec's own tests). The child process runs helperAcquireLockMain,
// acquires the real lock via the package's own TryAcquireSessionLock,
// and reports back over stdout so the parent test can synchronize
// without sleeping blindly.

const helperProcessEnv = "CRUSH_LOCK_HELPER_PROCESS"

// TestHelperLockHold is not a real test — it's the re-exec entry point
// invoked by spawnLockHolder via `-test.run=^TestHelperLockHold$`. Under
// a normal `go test` run (without the sentinel env var set) it does
// nothing and returns immediately.
//
// Protocol on stdout, one line each, unbuffered:
//
//	LOCKED <holder-pid>   -- once the lock is acquired
//	FAILED <error>        -- if acquisition failed
//
// Then the process blocks until stdin is closed (parent closes its
// write end / kills the child) or the optional hold duration elapses,
// whichever comes first. This lets the parent test deterministically
// know the moment the lock is actually held, instead of guessing with a
// fixed sleep.
func TestHelperLockHold(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		return
	}
	// From here on this is not really a *testing.T-driven test; we are
	// running as a plain child process that happens to have been
	// launched via `go test -test.run=...`. Use os.Exit, not t.Fatal,
	// so the exit code is meaningful to the parent.
	dataDir := os.Getenv("CRUSH_LOCK_HELPER_DATADIR")
	sessionID := os.Getenv("CRUSH_LOCK_HELPER_SESSIONID")
	holdSeconds := os.Getenv("CRUSH_LOCK_HELPER_HOLD_SECONDS")

	if dataDir == "" || sessionID == "" {
		fmt.Println("FAILED missing-env")
		os.Exit(2)
	}

	lk, err := TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("LOCKED %d\n", lk.HolderPID)

	var holdFor time.Duration
	if holdSeconds != "" {
		if secs, perr := time.ParseDuration(holdSeconds + "s"); perr == nil {
			holdFor = secs
		}
	}

	if holdFor > 0 {
		time.Sleep(holdFor)
		_ = lk.Release()
		os.Exit(0)
	}

	// No hold duration: block "forever" (until killed by the parent, or
	// until stdin is closed — whichever the parent uses to end the
	// test). Reading from stdin blocks until EOF/close, which the
	// parent triggers by closing the pipe or killing the process.
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}

// lockHolder represents a spawned real OS process that holds (or tried
// to hold) a SessionLock via TryAcquireSessionLock.
type lockHolder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	pid    int
	locked bool
	failed string
}

// spawnLockHolder starts a real child process (re-exec of this test
// binary) that attempts TryAcquireSessionLock(dataDir, sessionID) and
// reports back whether it succeeded. If holdSeconds > 0, the child
// releases and exits on its own after that many seconds; otherwise it
// holds until killed or its stdin is closed.
//
// Blocks until the child has reported LOCKED or FAILED (bounded by a
// generous timeout), so callers never need to sleep-and-hope.
func spawnLockHolder(t *testing.T, dataDir, sessionID string, holdSeconds int) *lockHolder {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestHelperLockHold$")
	cmd.Env = append(os.Environ(),
		helperProcessEnv+"=1",
		"CRUSH_LOCK_HELPER_DATADIR="+dataDir,
		"CRUSH_LOCK_HELPER_SESSIONID="+sessionID,
	)
	if holdSeconds > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("CRUSH_LOCK_HELPER_HOLD_SECONDS=%d", holdSeconds))
	}

	stdinW, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	// Drain stderr so `go test -v` output from the child doesn't block
	// the pipe buffer; we don't assert on it.
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	h := &lockHolder{cmd: cmd, stdin: stdinW, pid: cmd.Process.Pid}

	// Read the first protocol line with a bounded timeout so a broken
	// helper can't hang the whole test suite.
	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			lineCh <- line
			return
		}
		lineCh <- ""
	}()

	select {
	case line := <-lineCh:
		switch {
		case len(line) >= 6 && line[:6] == "LOCKED":
			h.locked = true
		case len(line) >= 6 && line[:6] == "FAILED":
			h.failed = line
		default:
			h.failed = "no protocol line: " + line
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("timed out waiting for helper process to report lock status")
	}

	return h
}

// stop kills the helper process (if still running) and waits for it to
// exit, so the OS lock is guaranteed released before the test proceeds.
func (h *lockHolder) stop(t *testing.T) {
	t.Helper()
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
}

// release asks the helper to exit cleanly (closing stdin unblocks its
// blocking read) rather than killing it, then waits for exit.
func (h *lockHolder) release(t *testing.T) {
	t.Helper()
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
}

func lockPathFor(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "locks", "session-"+sanitiseSessionID(sessionID)+".lock")
}
