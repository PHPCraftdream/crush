package tools

import (
	"runtime"
	"strings"

	"github.com/charmbracelet/crush/internal/shell"
)

var safeCommands = []string{
	// Bash builtins and core utils
	//
	// Deliberately excludes command-wrapper utilities (env, nice, nohup,
	// time, timeout) even though they can be invoked read-only-ish (bare
	// `env` just prints the environment): isSafeReadOnly in bash.go
	// matches on prefix alone, so "env rm -rf ./secrets", "timeout 10
	// ./exfil.sh", "nice npm install" etc. would ALL match the "env"/
	// "timeout"/"nice" prefix and skip permissions.Request entirely,
	// letting the wrapped, genuinely arbitrary subcommand run unchecked —
	// found by a full-project @crush --role reviewer audit. None of these
	// wrappers have a legitimate read-only use case worth this risk; an
	// agent that needs the environment can use `printenv` (already safe,
	// takes no subcommand) instead of bare `env`.
	"cal",
	"date",
	"df",
	"du",
	"echo",
	"free",
	"groups",
	"hostname",
	"id",
	"kill",
	"killall",
	"ls",
	"printenv",
	"ps",
	"pwd",
	"set",
	"top",
	"type",
	"uname",
	"unset",
	"uptime",
	"whatis",
	"whereis",
	"which",
	"whoami",

	// Git
	"git blame",
	"git branch",
	"git config --get",
	"git config --list",
	"git describe",
	"git diff",
	"git grep",
	"git log",
	"git ls-files",
	"git ls-remote",
	"git remote",
	"git rev-parse",
	"git shortlog",
	"git show",
	"git status",
	"git tag",
}

// isSafeReadOnlyCommand reports whether command matches safeCommands closely
// enough to skip the permission prompt entirely (see safeCommands's own doc
// for why command-wrapper utilities are deliberately excluded from that
// list). Command-chaining detection lives in shell.IsCompoundCommand (a
// real shell parse) — a compound command (chaining, backgrounding,
// substitution, subshell) always goes through the permission prompt, so a
// safe prefix can never smuggle a second command (e.g. "git status && rm -rf
// /", or "git status\nrm -rf /").
func isSafeReadOnlyCommand(command string) bool {
	if shell.IsCompoundCommand(command) {
		return false
	}
	cmdLower := strings.ToLower(command)
	for _, safe := range safeCommands {
		if strings.HasPrefix(cmdLower, safe) {
			if len(cmdLower) == len(safe) || cmdLower[len(safe)] == ' ' || cmdLower[len(safe)] == '-' {
				return true
			}
		}
	}
	return false
}

func init() {
	if runtime.GOOS == "windows" {
		safeCommands = append(
			safeCommands,
			// Windows-specific commands
			"ipconfig",
			"nslookup",
			"ping",
			"systeminfo",
			"tasklist",
			"where",
		)
	}
}
