package session

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestKillProcess_Live spawns a real long-running child, asks KillProcess
// to terminate it, and confirms IsProcessAlive reports false within a
// short window. Cross-platform because the cmd choice is platform-aware.
func TestKillProcess_Live(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd.exe", "/c", "ping", "-n", "30", "127.0.0.1")
	default:
		cmd = exec.Command("sleep", "30")
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(pid) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if IsProcessAlive(pid) {
		t.Fatalf("PID %d still alive 5s after KillProcess", pid)
	}
	_, _ = cmd.Process.Wait()
}

func TestKillProcess_AlreadyDead(t *testing.T) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd.exe", "/c", "exit", "0")
	default:
		cmd = exec.Command("true")
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
