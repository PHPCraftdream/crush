//go:build windows

package mcp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// helperEnvVar switches this test binary into one of two "leaky process
// tree" helper modes (see TestMain) instead of running the normal test
// suite. Standard Go idiom for reproducing real OS-level process/handle
// inheritance deterministically; mirrors internal/shell/exec_windows_test.go,
// which pins the same class of bug for background shell commands.
const helperEnvVar = "CRUSH_MCP_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnvVar) {
	case "level1":
		runLevel1Helper()
		return
	case "level2":
		runLevel2Helper()
		return
	}
	os.Exit(m.Run())
}

// runLevel1Helper models a stdio MCP server that spawns its own child (an
// npx-based server launching node, or signal-mcp launching signal-cli). It
// forks a grandchild (level 2) that inherits its stdout handle without
// waiting for it, then stays alive like a real server until killed.
func runLevel1Helper() {
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	cmd := exec.CommandContext(context.Background(), self)
	cmd.Env = append(os.Environ(), helperEnvVar+"=level2")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	// Fire-and-forget: do NOT Wait() on the grandchild — it must outlive
	// this process's own death exactly like an orphaned worker would.
	fmt.Println("level1-alive")
	time.Sleep(30 * time.Second)
}

// runLevel2Helper models the orphaned grandchild: it inherited level 1's
// stdout handle and keeps it open long after level 1 is gone. It reports its
// own PID so the test can confirm it, specifically, was reaped.
func runLevel2Helper() {
	fmt.Printf("grandchild-alive pid=%d\n", os.Getpid())
	time.Sleep(60 * time.Second)
}

// TestConfigureStdioProcess_CancelTreeKillsOrphanedGrandchild_Windows proves
// that cancelling a stdio MCP child's context — as happens on ClientSession
// Close, a StateError transition, or a lazy renew — tree-kills a grandchild
// the direct child spawned, instead of leaving it orphaned.
//
// Before this fix, cmd.Cancel used os/exec's default behavior of
// cmd.Process.Kill(), which terminates only the DIRECT child. That matches
// exactly what the go-sdk's own pipeRWC.Close (mcp.CommandTransport's normal
// shutdown path) already does — s.cmd.Process.Signal(syscall.SIGTERM) isn't
// supported on Windows, so it falls straight through to Process.Kill(), the
// direct process only. Either path leaves a grandchild the stdio server
// spawned (e.g. an npx-based server launching node) running forever with a
// dead parent — the same class of leak internal/shell/exec_windows.go fixed
// for background shell commands.
func TestConfigureStdioProcess_CancelTreeKillsOrphanedGrandchild_Windows(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, self)
	cmd.Env = append(os.Environ(), helperEnvVar+"=level1")
	configureStdioProcess(cmd)

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	var (
		mu     sync.Mutex
		output strings.Builder
	)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			mu.Lock()
			output.WriteString(scanner.Text())
			output.WriteString("\n")
			mu.Unlock()
		}
	}()

	require.NoError(t, cmd.Start())

	var grandchildPID int
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		s := output.String()
		if !strings.Contains(s, "level1-alive") {
			return false
		}
		idx := strings.Index(s, "grandchild-alive pid=")
		if idx < 0 {
			return false
		}
		line := s[idx+len("grandchild-alive pid="):]
		if nl := strings.IndexAny(line, "\r\n"); nl >= 0 {
			line = line[:nl]
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return false
		}
		grandchildPID = pid
		return true
	}, 5*time.Second, 50*time.Millisecond, "helper processes did not report alive")

	require.True(t, session.IsProcessAlive(grandchildPID), "grandchild must be alive before cancel")

	start := time.Now()
	cancel()
	_ = cmd.Wait() // Cancel makes Wait return a non-nil error; that's expected.
	elapsed := time.Since(start)

	// The kill path itself (configureStdioProcess's cmd.Cancel) is a single
	// synchronous `taskkill /F /T /PID` subprocess call — no polling loop,
	// no retries, no artificial sleep before it fires. On an idle machine
	// this whole test (spawn two helper levels, observe both alive, cancel,
	// Wait) completes in well under a second. The bound below is therefore
	// about host scheduling latency, not kill-path correctness: under a full
	// `go test ./...` run with dozens of subprocess-heavy packages executing
	// in parallel (including cliprovider's own high-volume subprocess stress
	// tests), spawning and waiting on the taskkill helper process can itself
	// be delayed by CPU/scheduler contention, observed once at ~15.2s. 25s
	// keeps real regressions caught fast (a broken kill path degrades to the
	// grandchild's 60s sleep, ~4x this bound) while giving headroom for
	// CPU-starved full-suite runs; it is not a "just in case" bump to a
	// large fixed ceiling.
	require.Less(t, elapsed, 25*time.Second,
		"Wait must return promptly once the process tree is killed")

	require.Eventually(t, func() bool {
		return !session.IsProcessAlive(grandchildPID)
	}, 5*time.Second, 100*time.Millisecond,
		"grandchild must be tree-killed when the stdio child's context is cancelled, not left orphaned")
}
