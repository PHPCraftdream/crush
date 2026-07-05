package shell

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// IsCompoundCommand reports whether cmd is anything other than a single
// simple command — i.e. it chains (`;`, a raw newline, `&&`, `||`, `|`),
// backgrounds (`&`), substitutes (`$(...)`, backticks), or opens a
// subshell / block / control structure.
//
// It parses cmd with the same shell grammar this package executes
// (mvdan.cc/sh/v3) rather than scanning for a fixed set of metacharacter
// substrings, so it can't be fooled by a metacharacter the substring
// scan happened to miss (a raw newline, a bare backgrounding `&`) and it
// does NOT trip on a metacharacter that only appears inside quotes
// (`echo 'a; b'` is a single command) or in a plain redirection
// (`ls 2>&1` operates on one command). A command that fails to parse is
// reported as compound: callers gating a security-sensitive fast-path
// (skip a permission prompt, or approve a command by a prefix pattern)
// must err toward "not simple" for anything they can't prove is a single
// command.
func IsCompoundCommand(cmd string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return true
	}
	if len(file.Stmts) != 1 {
		return true
	}
	stmt := file.Stmts[0]
	if stmt.Background {
		return true
	}
	// Anything that isn't a single simple command (a pipeline / `&&` /
	// `||` BinaryCmd, a subshell, a block, an if/for/while, …) is compound.
	if _, ok := stmt.Cmd.(*syntax.CallExpr); !ok {
		return true
	}
	// Reject command / process substitution anywhere in the words or
	// redirections of the single command.
	compound := false
	syntax.Walk(stmt, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			compound = true
			return false
		}
		return true
	})
	return compound
}
