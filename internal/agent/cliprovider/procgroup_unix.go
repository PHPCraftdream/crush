//go:build !windows

package cliprovider

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/crush/internal/session"
)

// trackChildTree is the Unix counterpart of the Windows Job Object
// registration in procgroup_windows.go. The ORDINARY teardown paths need
// nothing from it — KillProcess detects process-group leaders itself via
// getpgid — but registering the child lets the last-resort sweep in
// session.KillAllTrackedTrees find it when crush is about to die without
// running its own cleanup (the second Ctrl-C; see internal/cmd/run.go).
// Windows gets that case for free from KILL_ON_JOB_CLOSE; Unix does not.
//
// session.TrackProcessTree records only pids that are their own group
// leader, so a child spawned without Setpgid is silently not registered —
// which is correct: there is no group of ours to sweep.
func trackChildTree(proc *os.Process) int {
	if proc == nil {
		return 0
	}
	_ = session.TrackProcessTree(proc.Pid)
	return proc.Pid
}

// configureChildProcessGroup makes a pipe-branch CLI child a
// process-group leader (pgid == pid) so session.KillProcess can kill
// its whole tree — the direct child plus every grandchild it spawns
// (claude.cmd → node.exe chains, MCP servers the CLI itself starts, …)
// — with one killpg. This mirrors internal/agent/tools/mcp/
// process_unix.go, where the same orphan-leak shape on stdio MCP
// servers was fixed the same way.
//
// The PTY branch must NOT grow this: go-pty's unixPty.start() already
// sets SysProcAttr{Setsid: true, Setctty: true}, and a session leader
// is by definition a process-group leader, so KillProcess's group-kill
// path applies there unconditionally.
func configureChildProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
