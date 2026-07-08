//go:build !windows

package cmd

// maybeDetachConsole is a no-op on non-Windows platforms. POSIX has no
// console-close force-termination; a detached child simply gets SIGHUP,
// which orchestrators already avoid via nohup/setsid, and which Go ignores
// by default unless subscribed.
func maybeDetachConsole() {}
