//go:build !windows

package cmd

// installConsoleCtrlFilter is a no-op on non-Windows platforms. The
// spurious-cancellation-on-console-close issue this works around (see the
// windows-build file's doc comment) is specific to how Go's runtime maps
// Windows console control events onto os.Interrupt — POSIX SIGINT/SIGTERM
// don't have that ambiguity.
func installConsoleCtrlFilter() (uninstall func()) {
	return func() {}
}
