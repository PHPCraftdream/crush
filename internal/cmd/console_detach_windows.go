//go:build windows

package cmd

import (
	"os"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

var (
	procFreeConsole      = kernel32DLL.NewProc("FreeConsole")
	procAllocConsole     = kernel32DLL.NewProc("AllocConsole")
	procGetConsoleWindow = kernel32DLL.NewProc("GetConsoleWindow")

	user32DLL      = windows.NewLazySystemDLL("user32.dll")
	procShowWindow = user32DLL.NewProc("ShowWindow")
)

const swHide = 0

// maybeDetachConsole gives `crush run` its OWN hidden console when ALL
// THREE standard streams are redirected (not a terminal) — the
// orchestrator launch pattern (`crush run < prompt > out 2> err`, often
// backgrounded with `&` from a wrapper shell that exits instantly).
//
// Why FreeConsole+AllocConsole, not just FreeConsole: when the wrapper
// shell exits and ITS console goes away, Windows sends CTRL_CLOSE_EVENT to
// every process still attached to that console — and for CTRL_CLOSE_EVENT
// the process is ALWAYS terminated once the handler chain returns; handling
// the event (installConsoleCtrlFilter) only suppresses the confirmation
// UI, the kill itself cannot be prevented from inside a handler. Observed
// in the wild: SessionAgent.Run started, lock created, dead before the
// first heartbeat tick, zero stderr, zero log entries — the OS
// TerminateProcess()'d it mid-boot.
//
// A bare FreeConsole() (no console at all) fixes the kill, but has a side
// effect: mvdan.cc/sh's DefaultExecHandler (which runs every bash-tool
// command) spawns children via a bare exec.Cmd with no SysProcAttr set —
// upstream code we don't control. With no console to inherit, Windows
// allocates a FRESH, visible console for every single tool invocation,
// which flashes on screen and off again per command. AllocConsole() right
// after FreeConsole() gives crush its own console — owned by crush, not
// the dying wrapper, so CTRL_CLOSE_EVENT from the wrapper's death can't
// reach it — which child processes then inherit instead of each spawning
// their own. Hiding that console's window (ShowWindow SW_HIDE) keeps it
// invisible throughout.
//
// The redirect-target file handles are not console handles, so they
// survive both calls untouched and output keeps flowing into the redirect
// files.
//
// When any stream IS a terminal (an operator typed `crush run ...`
// interactively), we do none of this: Ctrl+C must keep working normally,
// and closing your own terminal tab ending the run is expected behavior.
func maybeDetachConsole() {
	if term.IsTerminal(int(os.Stdin.Fd())) ||
		term.IsTerminal(int(os.Stdout.Fd())) ||
		term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	_, _, _ = procFreeConsole.Call()
	if r1, _, _ := procAllocConsole.Call(); r1 == 0 {
		// AllocConsole failed (rare) — nothing more we can do; the run
		// proceeds console-less, same as the earlier bare-FreeConsole
		// behavior (still immune to CTRL_CLOSE_EVENT, just with the
		// per-tool-call window flash this function otherwise avoids).
		return
	}
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		_, _, _ = procShowWindow.Call(hwnd, swHide)
	}
}
