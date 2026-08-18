//go:build !windows

package cliprovider

import (
	"os"
	"os/exec"
	"syscall"
)

// trackChildTree is the Unix counterpart of the Windows Job Object
// registration in procgroup_windows.go: there is nothing to register at
// runtime (KillProcess detects process-group leaders itself via
// getpgid), so it only returns the pid for the matching
// UntrackProcessTree call.
func trackChildTree(proc *os.Process) int {
	if proc == nil {
		return 0
	}
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
