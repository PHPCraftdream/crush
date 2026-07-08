//go:build windows

package cmd

import (
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	kernel32DLL               = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32DLL.NewProc("SetConsoleCtrlHandler")
)

// installConsoleCtrlFilter installs a Windows console-control handler that
// swallows CTRL_CLOSE_EVENT / CTRL_LOGOFF_EVENT / CTRL_SHUTDOWN_EVENT so
// `crush run` invoked directly in a foreground terminal is not cancelled by
// a window-close/logoff/shutdown console event — only a genuine
// CTRL_C_EVENT or CTRL_BREAK_EVENT (real Ctrl+C / Ctrl+Break) still reaches
// Go's os/signal machinery and cancels the run's context.
//
// Why this is needed: Go's runtime installs its own low-level console
// control handler that turns ALL FIVE event types into signal.Notify's
// os.Interrupt uniformly — by the time it reaches os/signal there is no way
// to tell "user pressed Ctrl+C" apart from "the console window got a
// close/logoff/shutdown event" (a routine thing: alt-tabbing away in some
// terminal hosts, a Windows Terminal tab close, etc.). Without this, a
// plain `crush run --session X "message"` typed directly in a terminal —
// exactly the everyday interactive use case, no stdin redirection or
// backgrounding involved — can abort mid-turn with "Context canceled" for
// no operator-visible reason.
//
// Handlers registered via SetConsoleCtrlHandler are invoked in LIFO order
// and a TRUE return stops the chain, so registering AFTER Go's runtime
// (which happens automatically at process init) lets us intercept and
// suppress the non-interactive event types before Go's own handler — and
// therefore os/signal — ever sees them. CTRL_C_EVENT/CTRL_BREAK_EVENT fall
// through unhandled (return FALSE) so real Ctrl+C keeps working exactly as
// before.
//
// Returns an uninstall func; safe to call even if the underlying
// SetConsoleCtrlHandler call failed (e.g. crush isn't actually attached to
// a console — the common case for a headless orchestrator launch) — the
// handler simply never fires in that case.
func installConsoleCtrlFilter() (uninstall func()) {
	handler := syscall.NewCallback(func(ctrlType uint32) uintptr {
		switch ctrlType {
		case windows.CTRL_CLOSE_EVENT, windows.CTRL_LOGOFF_EVENT, windows.CTRL_SHUTDOWN_EVENT:
			// Handled/swallowed: Go's runtime handler (and thus
			// os/signal's os.Interrupt delivery) never runs for these.
			return 1
		default:
			// CTRL_C_EVENT / CTRL_BREAK_EVENT — not handled here, let
			// Go's own handler process it normally.
			return 0
		}
	})
	r1, _, _ := procSetConsoleCtrlHandler.Call(handler, 1)
	if r1 == 0 {
		return func() {}
	}
	return func() {
		_, _, _ = procSetConsoleCtrlHandler.Call(handler, 0)
	}
}
