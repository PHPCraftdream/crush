package platform

import (
	"context"
	"os/exec"
)

// Command builds an *exec.Cmd that is ALREADY hardened for the current
// platform, and is the single sanctioned way to spawn a child process
// anywhere inside crush. Callers configure the returned command as usual
// (Dir, Env, Stdin/Stdout/Stderr, …) — only the platform-specific process
// creation attributes are pre-set.
//
// On Windows the hardening is [HideConsoleWindow]: without it a
// console-subsystem child (rg, git, npm.cmd, node, taskkill, …) pops a
// real console window whenever crush itself has no console to share —
// which is the normal state for an orchestrator/detached `crush run`,
// see cmd.maybeDetachConsole. Those windows flash on screen, cover the
// operator's work and steal keyboard focus. On every other platform the
// hardening is a no-op and this is a thin wrapper over
// [exec.CommandContext].
//
// Why a constructor rather than a "remember to call HideConsoleWindow"
// convention: the convention was in force since d630d3a3 and depended on
// every future call site remembering one extra line, with no test to
// catch an omission. TestNoUnhardenedProcessSpawns (in this package)
// now enforces that no code under internal/ reaches for
// exec.Command/exec.CommandContext directly, so a new spawn cannot
// silently regress the fix.
//
// NOT covered by this constructor, deliberately: the ConPTY spawn in
// internal/agent/cliprovider (go-pty's own Cmd, not an *exec.Cmd).
// go-pty hand-rolls CreateProcess on Windows and ignores
// SysProcAttr.HideWindow entirely, while CreationFlags |= CREATE_NO_WINDOW
// — the only knob it does honour — was measured to break the pseudo
// console outright (247 bytes of child output without it, 0 bytes with
// it). A ConPTY is headless by construction and was confirmed by
// window-enumeration tracing not to create a visible window, so it needs
// no suppression.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	HideConsoleWindow(cmd)
	return cmd
}
