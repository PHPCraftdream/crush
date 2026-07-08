//go:build windows

package platform

import (
	"os/exec"
	"syscall"
)

// HideConsoleWindow sets SysProcAttr.HideWindow so a spawned console-
// subsystem child (rg, npm.cmd, node, git, ...) doesn't briefly flash a
// visible console window. Needed whenever crush itself may have no
// console of its own to share — e.g. a detached/orchestrator run (see
// cmd.maybeDetachConsole) — in which case Windows would otherwise allocate
// a fresh console per spawn. Safe to call unconditionally: it's a no-op
// UX improvement even when crush does have a console (redirected output
// keeps flowing to Stdout/Stderr either way, only the window is
// suppressed).
func HideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
