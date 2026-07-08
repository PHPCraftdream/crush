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
// with `&` from a wrapper shell that exits immediately).
//
// Why: when the wrapper shell exits and its console goes away, Windows
// sends CTRL_CLOSE_EVENT to every process attached to that console — and
// for CTRL_CLOSE_EVENT the process is ALWAYS terminated after the handler
// chain returns. Handling the event (see installConsoleCtrlFilter) only
// suppresses the confirmation UI; the kill itself cannot be prevented from
// inside a handler. Observed as: a detached `crush run` dying silently
// ~5-15s after its wrapper exited, with an orphan session lock, zero
// stderr output, and no log entries — the OS TerminateProcess()'d it
// mid-boot or mid-stream.
//
// The only real escape is to not be attached to the dying console at all:
// FreeConsole(). With no console there is no CTRL_CLOSE_EVENT, no console
// ctrl anything — the process lives until it finishes or is killed
// explicitly (taskkill / `crush sessions kill`), which is exactly the
// contract a detached orchestrator run wants. --timeout and the hard-kill
// backstop still bound the runtime. The redirect-target file handles are
// not console handles, so they survive FreeConsole untouched and output
// keeps flowing into the redirect files.
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
