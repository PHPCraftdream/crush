package session

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
)

// TestKillProcess_Live spawns a real long-running child, asks KillProcess
// to terminate it, and confirms IsProcessAlive reports false within a
// short window. Cross-platform because the cmd choice is platform-aware.
func TestKillProcess_Live(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(context.Background(), "cmd.exe", "/c", "ping", "-n", "30", "127.0.0.1")
	default:
		cmd = exec.CommandContext(context.Background(), "sleep", "30")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn child process for kill test: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	pid := cmd.Process.Pid
	if !IsProcessAlive(pid) {
		t.Fatalf("child PID %d not alive after Start", pid)
	}
	if err := KillProcess(pid); err != nil {
		t.Fatalf("KillProcess(%d): %v", pid, err)
	}

	// Reap the killed child so it is not left as a zombie. On Unix a zombie
	// still answers kill(pid, 0), so IsProcessAlive would report it alive
	// until the parent (this test) waits on it. Wait() returns promptly once
	// SIGKILL lands; on Windows it returns after the process terminates.
	_, _ = cmd.Process.Wait()
	if IsProcessAlive(pid) {
		t.Fatalf("PID %d still alive after KillProcess + reap", pid)
	}
}

func TestKillProcess_AlreadyDead(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(context.Background(), "cmd.exe", "/c", "exit", "0")
	default:
		cmd = exec.CommandContext(context.Background(), "true")
	}
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run trivial child: %v", err)
	}
	pid := cmd.Process.Pid
	// Race-tolerant: process may already be reaped, but we still expect
	// KillProcess to return nil for a long-gone PID.
	if err := KillProcess(pid); err != nil {
		t.Fatalf("KillProcess on dead PID %d should be nil, got: %v", pid, err)
	}
}

func TestKillProcess_InvalidPID(t *testing.T) {
	if err := KillProcess(0); err == nil {
		t.Fatal("KillProcess(0) should error")
	}
	if err := KillProcess(-1); err == nil {
		t.Fatal("KillProcess(-1) should error")
	}
}
