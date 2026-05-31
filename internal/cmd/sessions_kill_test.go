package cmd

import (
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
		cmd = exec.Command("cmd.exe", "/c", "exit", "0")
	default:
		cmd = exec.Command("true")
	}
	require.NoError(t, cmd.Run())
	report := forceKillHolder(cmd.Process.Pid, time.Second)
	assert.Contains(t, report, "already gone")
}

func TestForceKillHolder_LiveProcess(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd.exe", "/c", "ping", "-n", "30", "127.0.0.1")
	default:
		cmd = exec.Command("sleep", "30")
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
