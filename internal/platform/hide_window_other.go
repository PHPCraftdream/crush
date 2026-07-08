//go:build !windows

package platform

import "os/exec"

// HideConsoleWindow is a no-op on non-Windows platforms — there is no
// console window concept to suppress.
func HideConsoleWindow(_ *exec.Cmd) {}
