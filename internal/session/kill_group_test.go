//go:build !windows

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spawnTreeChild starts `sh -c "sleep 60 & echo $! > pidFile; wait"`
// and returns the cmd plus the file the grandchild's pid lands in. With
// setpgid the child is spawned as a process-group leader, mirroring how
// cliprovider and the MCP stdio client spawn their children; without it
// the child stays in this test process's group.
func spawnTreeChild(t *testing.T, setpgid bool) (*exec.Cmd, string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c",
		"sleep 60 & echo $! > '"+pidFile+"'; wait")
	if setpgid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sh child: %v", err)
	}
	return cmd, pidFile
}

// readGrandchildPid polls the pid file until the child writes the
// grandchild's pid, then returns it.
func readGrandchildPid(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild pid file %s never became readable", pidFile)
	return 0
}

// TestKillProcess_GroupLeaderKillsGrandchild is the Unix regression
// test for the orphaned-grandchild defect: KillProcess must kill the
// whole process group when (and only when) the target pid is that
// group's leader, which is how cliprovider and the MCP stdio client
// spawn their children (Setpgid). REVERT CHECK: with KillProcess
// reverted to a single-process SIGKILL, the grandchild survives and
// this fails on the "still alive" assertion below.
func TestKillProcess_GroupLeaderKillsGrandchild(t *testing.T) {
	cmd, pidFile := spawnTreeChild(t, true)
	grandpid := readGrandchildPid(t, pidFile)
	// Pid-based cleanup: safe under -p 4 parallelism (no name matching,
	// so no collision with other packages' sleeps) and runs even when
	// an assertion below fails.
	t.Cleanup(func() { _ = syscall.Kill(grandpid, syscall.SIGKILL) })

	if !IsProcessAlive(grandpid) {
		t.Fatalf("grandchild %d not alive before KillProcess — test premise broken", grandpid)
	}
	if err := KillProcess(cmd.Process.Pid); err != nil {
		t.Fatalf("KillProcess(child %d): %v", cmd.Process.Pid, err)
	}
	// Reap the killed leader so it does not linger as a zombie.
	_, _ = cmd.Process.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(grandpid) {
			return // grandchild died with the group — fix verified
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d still alive after KillProcess of group leader %d — group kill regressed", grandpid, cmd.Process.Pid)
}

// TestKillProcess_NonLeaderKillsOnlyTarget pins the safety half of the
// Unix fix: for a pid that is NOT a process-group leader (any foreign
// pid, or a child spawned without Setpgid), KillProcess must fall back
// to the single-process kill and must never signal a process group.
// The grandchild here shares this test process's own group; a
// regression to an unconditional kill(-pid) would, best case, miss the
// target entirely (the numeric group does not exist) and leave the
// child un-killed.
func TestKillProcess_NonLeaderKillsOnlyTarget(t *testing.T) {
	cmd, pidFile := spawnTreeChild(t, false)
	grandpid := readGrandchildPid(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(grandpid, syscall.SIGKILL) })

	if err := KillProcess(cmd.Process.Pid); err != nil {
		t.Fatalf("KillProcess(child %d): %v", cmd.Process.Pid, err)
	}
	_, _ = cmd.Process.Wait()
	if IsProcessAlive(cmd.Process.Pid) {
		t.Fatalf("child %d still alive after KillProcess", cmd.Process.Pid)
	}
	// The grandchild is not in any group named after the child's pid,
	// so nothing but the explicit cleanup kill may take it down.
	if !IsProcessAlive(grandpid) {
		t.Fatalf("grandchild %d died although the target was not a group leader — KillProcess signalled a group it must not have", grandpid)
	}
}
