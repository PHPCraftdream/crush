package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCompoundCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		// Single simple commands — not compound.
		{"plain ls", "ls -la", false},
		{"plain echo", "echo hello world", false},
		{"plain pwd", "pwd", false},
		{"plain git status", "git status", false},
		{"ls with redirect", "ls > /tmp/out", false},
		{"ls with &> redirect", "ls &> /dev/null", false},
		{"ls with >& redirect", "ls >& /dev/null", false},
		{"ls with 2>&1 redirect", "ls 2>&1", false},
		{"simple kill", "kill 1234", false},
		{"git log", "git log --oneline", false},
		{"dollar sign in argument", "echo $HOME", false},
		// A chaining metacharacter inside quotes is a literal, not a chain.
		{"quoted semicolon", "echo 'a; b'", false},

		// Chaining / substitution — compound.
		{"ls with pipe", "ls | grep foo", true},
		{"ls with &&", "ls && echo done", true},
		{"ls with semicolon", "ls; echo done", true},
		{"ls with ||", "ls || echo fail", true},
		{"ls with backticks", "ls `echo foo`", true},
		{"ls with subshell subst", "ls $(echo foo)", true},
		{"rm then ls via &&", "rm -rf / && ls", true},
		{"kill with pipe", "kill 1234 | echo foo", true},
		{"git log with pipe", "git log | head", true},
		{"subshell", "(rm -rf /)", true},

		// Regression guards: the old substring scans (permission's and the
		// bash tool's) missed these.
		{"backgrounding ampersand chain", "ls & echo done", true},
		{"lone background", "sleep 60 &", true},
		{"newline chain", "git status\nrm -rf /", true},

		// Unparseable input errs toward compound (deny-safe).
		{"unterminated quote", "echo 'unterminated", true},
		// Empty / whitespace-only is not a single command, so it is
		// reported as not-simple (deny-safe). Both callers pre-filter empty
		// commands, so this only pins the standalone contract.
		{"empty string", "", true},
		{"whitespace only", "   ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, IsCompoundCommand(tt.input), "IsCompoundCommand(%q)", tt.input)
		})
	}
}
