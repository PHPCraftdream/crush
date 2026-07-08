package cmd

import "testing"

// TestInstallConsoleCtrlFilter is a smoke test: installing and immediately
// uninstalling must not panic and must return a non-nil uninstall func on
// every platform (a no-op on non-Windows, a real SetConsoleCtrlHandler
// round-trip on Windows). The interesting behavior (which event types get
// swallowed) is Windows-syscall-only and not meaningfully unit-testable
// without a real console, so this only pins the contract both build-tagged
// implementations must satisfy.
func TestInstallConsoleCtrlFilter(t *testing.T) {
	uninstall := installConsoleCtrlFilter()
	if uninstall == nil {
		t.Fatal("installConsoleCtrlFilter must return a non-nil uninstall func")
	}
	uninstall() // must not panic
}
