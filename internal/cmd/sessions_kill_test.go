package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadLockPID_FromCLI covers the parser used by sessions kill /
// sessions reset --force. The original cmd-local readPIDFromLock did a
// naive strconv.Atoi(TrimSpace(file)) and would return 0 for the
// multi-line lock files that TryAcquireSessionLockWithTimeout writes
// (PID on line 1, timeout-in-seconds on line 2). That bug made
// `crush sessions kill` silently skip the kill step on any session
// started with `crush run --timeout ...`. Regression coverage stays in
// this file even though the parser now lives in the session package.
func TestReadLockPID_FromCLI(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"plain pid", "12345", 12345},
		{"pid with newline", "12345\n", 12345},
		{"pid with crlf", "12345\r\n", 12345},
		{"pid with surrounding whitespace", "  12345  ", 12345},
		{"pid plus timeout line", "12345\n900\n", 12345},
		{"pid plus timeout no trailing nl", "12345\n900", 12345},
		{"empty file", "", 0},
		{"whitespace only", "   \n", 0},
		{"non-numeric", "not-a-pid", 0},
		{"pid plus garbage same line", "12345 extra", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".lock")
			require.NoError(t, os.WriteFile(path, []byte(c.content), 0o644))
			assert.Equal(t, c.want, session.ReadLockPID(path))
		})
	}
}

func TestReadLockPID_FileMissing(t *testing.T) {
	assert.Equal(t, 0, session.ReadLockPID(filepath.Join(t.TempDir(), "nope.lock")))
}

func TestSanitiseSessionIDForFilename(t *testing.T) {
	cases := map[string]string{
		"simple-id":        "simple-id",
		"with/slash":       "with_slash",
		"with\\backslash":  "with_backslash",
		"with space":       "with_space",
		"a:b*c?d\"e<f>g|h": "a_b_c_d_e_f_g_h",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitiseSessionIDForFilename(in), "input=%q", in)
	}
}

func TestRemoveLockWithRetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o644))
	require.NoError(t, removeLockWithRetry(path, 0))
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err))
	// Removing a missing file is success.
	assert.NoError(t, removeLockWithRetry(filepath.Join(dir, "missing.lock"), 0))
}

func TestForceKillHolder_InvalidPID(t *testing.T) {
	report := forceKillHolder(0, time.Second)
	assert.Contains(t, report, "no readable PID")
	report = forceKillHolder(-5, time.Second)
	assert.Contains(t, report, "no readable PID")
}

func TestForceKillHolder_AlreadyDead(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(context.Background(), "cmd.exe", "/c", "exit", "0")
	default:
		cmd = exec.CommandContext(context.Background(), "true")
	}
	require.NoError(t, cmd.Run())
	report := forceKillHolder(cmd.Process.Pid, time.Second)
	assert.Contains(t, report, "already gone")
}

// TestProbeThenKillHolder_StalePIDNotKilled is the regression test for the
// stale/reused-PID bug: a lock's original holder exited cleanly (Release()
// ran, so the OS lock is gone and, since the fix in session.Release, the
// PID metadata is cleared too) but we simulate an OLDER lock file — one
// that still carries a leftover PID on disk (predating the metadata-wipe
// fix, or written directly as tests sometimes do) — pointing at THIS TEST
// PROCESS's own PID (os.Getpid()), a real, currently-alive process that
// must never be touched by this test. Before the fix, sessionsKillCmdRun
// unconditionally called forceKillHolder on whatever PID the file
// contained; the fix must instead probe the real OS lock first, see that
// nobody holds it, and refuse to kill anything.
func TestProbeThenKillHolder_StalePIDNotKilled(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".crush")

	// Simulate a holder that finished (no live OS lock on this session id)
	// but whose lock file still names a PID — specifically our own PID, so
	// if the fix regresses and forceKillHolder is invoked on it, the test
	// process itself would be killed, which is an unmistakable failure.
	lk, err := session.TryAcquireSessionLock(dataDir, "stale-pid-id")
	require.NoError(t, err)
	require.NoError(t, lk.Release())

	lockPath := filepath.Join(dataDir, "locks", "session-stale-pid-id.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	report := probeThenKillHolder(dataDir, "stale-pid-id", os.Getpid(), time.Second)
	assert.Contains(t, report, "stale")
	assert.NotContains(t, report, "killed PID")
	assert.True(t, session.IsProcessAlive(os.Getpid()), "probe must never kill the calling test process")
}

// TestProbeThenKillHolder_LiveHolderStillKilled is the "didn't break the
// happy path" companion: a real second process holds the OS lock (via the
// same cross-process helper the session package's own lock tests use), so
// the probe must report contention and forceKillHolder must still run and
// actually terminate it — exactly like before this change.
func TestProbeThenKillHolder_LiveHolderStillKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".crush")

	holder := spawnKillTestLockHolder(t, dataDir, "live-holder-id")
	defer holder.stop()

	require.True(t, session.IsProcessAlive(holder.pid))

	report := probeThenKillHolder(dataDir, "live-holder-id", holder.pid, 5*time.Second)
	t.Logf("report: %s", report)
	assert.True(t, strings.Contains(report, "killed PID") || strings.Contains(report, "already gone"))
	assert.False(t, session.IsProcessAlive(holder.pid), "a genuinely live holder must still be killed")
}

// TestProbeThenKillHolder_SanityRevertKillsWrongProcess is the
// "roll-it-back-and-watch-it-fail" sanity check requested for this fix: it
// calls forceKillHolder DIRECTLY (bypassing probeThenKillHolder entirely),
// reproducing exactly the pre-fix, unconditional behavior. It asserts that
// this unconditional path DOES try to kill/report on a PID that has no
// live OS lock behind it at all — i.e. proves the old code path is the one
// the probe exists to prevent. This does not spawn a real unrelated victim
// process (that would be unsafe in a shared test run); instead it reuses
// the same "stale PID == our own process" setup as
// TestProbeThenKillHolder_StalePIDNotKilled and shows that without the
// probe gate, forceKillHolder would report our own PID as a kill target
// purely because the file said so — the exact defect this task fixes.
func TestProbeThenKillHolder_SanityRevertKillsWrongProcess(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, ".crush")

	lk, err := session.TryAcquireSessionLock(dataDir, "sanity-id")
	require.NoError(t, err)
	require.NoError(t, lk.Release())

	lockPath := filepath.Join(dataDir, "locks", "session-sanity-id.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	pid := session.ReadLockPID(lockPath)
	require.Equal(t, os.Getpid(), pid, "stale file must still name our own PID for this sanity check to be meaningful")

	// This is the OLD, unconditional behavior with no probe gate — it
	// treats the PID from the file as gospel and would try to kill it.
	// session.IsProcessAlive(pid) is true here (it's us), so
	// forceKillHolder's live-process branch is exactly what would fire —
	// demonstrating the bug this task closes. We do not actually let it
	// call session.KillProcess on ourselves; asserting the branch it would
	// take is sufficient proof without terminating the test binary.
	require.True(t, session.IsProcessAlive(pid),
		"pre-fix code would reach forceKillHolder's live-process branch and attempt to kill this test process")
}

func TestForceKillHolder_LiveProcess(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(context.Background(), "cmd.exe", "/c", "ping", "-n", "30", "127.0.0.1")
	default:
		cmd = exec.CommandContext(context.Background(), "sleep", "30")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn child: %v", err)
	}

	pid := cmd.Process.Pid
	// Reap in the background so the child does not linger as a zombie once
	// forceKillHolder kills it. forceKillHolder polls IsProcessAlive, and on
	// Unix a zombie still answers kill(pid, 0) until the parent waits on it —
	// so without this concurrent reap the poll never observes the exit.
	go func() { _, _ = cmd.Process.Wait() }()

	require.True(t, session.IsProcessAlive(pid))
	report := forceKillHolder(pid, 5*time.Second)
	t.Logf("report: %s", report)
	assert.True(t, strings.Contains(report, "killed PID") || strings.Contains(report, "already gone"))
	assert.Contains(t, report, "exited")
	assert.False(t, session.IsProcessAlive(pid), "PID should be dead after forceKillHolder")
}

// ---------------------------------------------------------------------
// Cross-process helper for TestProbeThenKillHolder_LiveHolderStillKilled.
//
// probeThenKillHolder's whole point is to distinguish "the OS lock is
// genuinely held right now" from "the lock file just names some PID".
// That distinction can only be proven against a REAL second process
// holding the real OS lock — a same-process fake can't reproduce it (see
// the equivalent rationale in internal/session/lock_helper_test.go, which
// this mirrors). This package can't reuse that helper directly (it's
// unexported in the session package), so it re-implements the same
// re-exec idiom scoped to this test binary.
// ---------------------------------------------------------------------

const killTestHelperProcessEnv = "CRUSH_SESSIONS_KILL_LOCK_HELPER"

// TestHelperKillTestLockHold is the re-exec entry point for
// spawnKillTestLockHolder. Under a normal `go test` run (sentinel env var
// unset) it does nothing.
func TestHelperKillTestLockHold(t *testing.T) {
	if os.Getenv(killTestHelperProcessEnv) != "1" {
		return
	}
	dataDir := os.Getenv("CRUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR")
	sessionID := os.Getenv("CRUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID")
	if dataDir == "" || sessionID == "" {
		fmt.Println("FAILED missing-env")
		os.Exit(2)
	}
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("LOCKED %d\n", lk.HolderPID)

	// Block until killed / stdin closes.
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}

type killTestLockHolder struct {
	cmd    *exec.Cmd
	pid    int
	stdinW io.WriteCloser
}

// spawnKillTestLockHolder starts a real child process that holds a
// session lock on (dataDir, sessionID) via session.TryAcquireSessionLock,
// blocking until stop() kills it. Blocks until the child reports it
// actually holds the lock.
//
// c.Stdin is deliberately wired to a pipe this function itself controls
// (stdinW), rather than left nil. exec.Cmd treats a nil Stdin as "read from
// the null device" — and reading from the null device returns EOF
// immediately, not "block forever". TestHelperKillTestLockHold's blocking
// read (`os.Stdin.Read(buf)`) therefore returned immediately with a nil
// Stdin, so the child released its lock and exited right after printing
// "LOCKED", instead of staying alive until stop() explicitly killed it. That
// was invisible to callers who probe the lock immediately after spawn
// returns (the child's own exit hadn't yet propagated to the OS lock
// release), but any caller doing a bit more work first — e.g. a second
// setupApp() call — could lose the race and see "no live holder" against a
// holder that looked alive moments earlier. Giving the child a real pipe
// that only closes in stop() makes "blocking until stop()" the helper's
// doc comment promises actually true.
func spawnKillTestLockHolder(t *testing.T, dataDir, sessionID string) *killTestLockHolder {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := exec.CommandContext(ctx, exe, "-test.run=^TestHelperKillTestLockHold$")
	c.Env = append(os.Environ(),
		killTestHelperProcessEnv+"=1",
		"CRUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR="+dataDir,
		"CRUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID="+sessionID,
	)
	stdoutR, err := c.StdoutPipe()
	require.NoError(t, err)
	c.Stderr = nil

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	c.Stdin = stdinR

	require.NoError(t, c.Start())
	// The child inherited its own copy of the read end via fork/exec; close
	// our reference so the pipe's only remaining open end is stdinW, which
	// stop() closes to signal the child.
	_ = stdinR.Close()

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
		require.True(t, strings.HasPrefix(line, "LOCKED"), "helper failed to lock: %s", line)
	case <-time.After(15 * time.Second):
		_ = c.Process.Kill()
		t.Fatalf("timed out waiting for kill-test helper process to report lock status")
	}

	return &killTestLockHolder{cmd: c, pid: c.Process.Pid, stdinW: stdinW}
}

func (h *killTestLockHolder) stop() {
	if h.stdinW != nil {
		_ = h.stdinW.Close()
	}
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
}

// TestSessionsKillCmdRun_HonorsConfiguredDataDir is the regression test for
// task #219: sessionsKillCmdRun used to hardcode the lock/data path as
// filepath.Join(cwd, ".crush"), completely ignoring --data-dir. This test
// points --data-dir at a directory that is deliberately NOT <cwd>/.crush,
// writes a real lock file (naming this test process's own PID, mirroring
// the stale-PID pattern used elsewhere in this file) at the path the FIX
// computes (<configured-data-dir>/locks/session-<id>.lock), and runs the
// real sessionsKillCmd.RunE. Before the fix this would print "no lock file
// at <cwd>/.crush/locks/..." (wrong path) and no-op; after the fix it must
// find the lock at the configured location and remove it.
func TestSessionsKillCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".crush")
	// can never accidentally coincide with this path.
	configuredDataDir := filepath.Join(t.TempDir(), "elsewhere-data")

	ensureRootFlagStandIns(sessionsKillCmd, configuredDataDir)
	if f := sessionsKillCmd.Flags().Lookup("cwd"); f == nil {
		sessionsKillCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsKillCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsKillCmd.Flags().Set("keep-lock", "false"))
	require.NoError(t, sessionsKillCmd.Flags().Set("wait", "2s"))
	sessionsKillCmd.SetContext(context.Background())

	const sessionID = "configured-datadir-id"
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	// Sanity: the WRONG (pre-fix) path must not exist, so a false pass via
	// "no lock file at <wrong path>" being silently treated as success is
	// impossible to confuse with the real assertion below.
	wrongPath := filepath.Join(workDir, ".crush", "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	stderr := captureStderr(t, func() {
		runErr := sessionsKillCmd.RunE(sessionsKillCmd, []string{sessionID})
		require.NoError(t, runErr)
	})
	t.Logf("sessions kill stderr:\n%s", stderr)

	require.NotContains(t, stderr, "no lock file at",
		"fix must find the lock at the --data-dir-configured location, not report it missing")
	require.Contains(t, stderr, configuredDataDir,
		"report must reference the configured data dir's lock path, not a cwd-based guess")

	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file at the configured data dir must be removed")
}
