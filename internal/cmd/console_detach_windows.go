//go:build windows

package cmd

import (
	"os"

	"golang.org/x/term"
)

var procFreeConsole = kernel32DLL.NewProc("FreeConsole")

// maybeDetachConsole detaches `crush run` from its console when ALL THREE
// standard streams are redirected (not a terminal) — the orchestrator
// launch pattern (`crush run < prompt > out 2> err`, often backgrounded
// with `&` from a wrapper shell that exits instantly).
//
// Why: when the wrapper shell exits and its console goes away, Windows
// sends CTRL_CLOSE_EVENT to every process still attached to that console —
// and for CTRL_CLOSE_EVENT the process is ALWAYS terminated once the
// handler chain returns; handling the event (installConsoleCtrlFilter)
// only suppresses the confirmation UI, the kill itself cannot be prevented
// from inside a handler. Observed in the wild: SessionAgent.Run started,
// lock created, dead before the first heartbeat tick, zero stderr, zero
// log entries — the OS TerminateProcess()'d it mid-boot.
//
// A bare FreeConsole (no console at all) is enough here: the earlier
// concern — mvdan.cc/sh's DefaultExecHandler spawning a new visible
// console per bash-tool command when crush itself has none to share — is
// now fixed at its own source (internal/shell/exec_windows.go sets
// SysProcAttr.HideWindow on every child it spawns), so crush doesn't need
// a console of its own for children to inherit. Giving crush its own
// console anyway (AllocConsole) was tried first and technically worked,
// but risks a brief visible flash before the immediate ShowWindow(SW_HIDE)
// takes effect — window creation and hiding aren't atomic. Not having a
// console at all has no such race.
//
// The redirect-target file handles are not console handles, so they
// survive FreeConsole untouched and output keeps flowing into the
// redirect files.
//
// When any stream IS a terminal (an operator typed `crush run ...`
// interactively), we stay attached: Ctrl+C must keep working, and dying
// with the terminal tab is the expected interactive behavior.
func maybeDetachConsole() {
	if term.IsTerminal(int(os.Stdin.Fd())) ||
		term.IsTerminal(int(os.Stdout.Fd())) ||
		term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	_, _, _ = procFreeConsole.Call()
}
