package agentguard

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheck_AllowsHarmless(t *testing.T) {
	for _, cmd := range []string{
		"",
		"ls -la",
		"go test ./...",
		"git status",
		"echo hello",
		"cat README.md",
		"node script.js",
		"python -c \"print(1)\"",
		"docker run --rm alpine echo hi",
		// shell wrapper around harmless content
		"bash -c 'go build .'",
		`cmd /c "echo hi"`,
		// npx with non-denied package
		"npx prettier --check src/",
		"pnpm dlx tsc --noEmit",
		"yarn dlx eslint .",
	} {
		t.Run(cmd, func(t *testing.T) {
			assert.NoError(t, Check(cmd))
		})
	}
}

func TestCheck_BlocksDirectAgents(t *testing.T) {
	cases := []string{
		"claude",
		"claude --print 'hi'",
		"claude.exe -p test",
		"claude.cmd",
		"Claude.EXE",
		"/usr/local/bin/claude",
		"./claude",
		"codex chat",
		"gemini -p hello",
		"qwen --model x",
		"opencode run",
		"aider --no-git",
		"cline",
		"cursor-agent",
		"crush",         // self
		"crush.exe run", // self with subcommand
		"./crush run something",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err)
			var de *DeniedError
			require.True(t, errors.As(err, &de), "expected DeniedError, got %T", err)
			assert.Contains(t, strings.ToLower(de.Error()), "blocked")
		})
	}
}

func TestCheck_BlocksThroughShellWrappers(t *testing.T) {
	cases := []string{
		`bash -c "claude --print hi"`,
		`sh -c 'claude -p test'`,
		`zsh -c "echo wrap; claude"`,
		`cmd /c "claude.cmd"`,
		`cmd.exe /c claude.exe`,
		`powershell -Command "claude --print hi"`,
		`pwsh -c claude`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err)
		})
	}
}

func TestCheck_BlocksThroughPackageRunners(t *testing.T) {
	cases := []string{
		"npx @anthropic-ai/claude-code -p hi",
		"npx -y @anthropic-ai/claude-code",
		"npx --yes @anthropic-ai/claude-code",
		"pnpm dlx @anthropic-ai/claude-code",
		"yarn dlx @anthropic-ai/claude-code",
		"bunx @anthropic-ai/claude-code",
		"bun x @anthropic-ai/claude-code",
		"npx @google/gemini-cli",
		"npx @opencode-ai/opencode",
		"pipx run aider-chat",
		"uvx aider-chat",
		// alias-style: bare name through npx (some publish under bare name)
		"npx claude",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err, "should block: %s", cmd)
		})
	}
}

func TestCheck_BlocksInsideChainedCommand(t *testing.T) {
	cases := []string{
		"echo step1 && claude",
		"echo step1 || claude",
		"echo step1; claude",
		"echo a | grep b | claude",
		`echo a && bash -c "claude -p go"`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err, "should block: %s", cmd)
		})
	}
}

func TestCheck_AllowsLeadingEnvAssignments(t *testing.T) {
	// env-style invocation: leading VAR=val pairs shouldn't fool the parser.
	err := Check("ANTHROPIC_API_KEY=x DEBUG=1 echo hi")
	assert.NoError(t, err)
}

func TestCheck_BlocksDespiteEnvAssignments(t *testing.T) {
	err := Check("ANTHROPIC_API_KEY=x claude -p test")
	require.Error(t, err)
	var de *DeniedError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "claude", de.Tool)
}

func TestCheck_BlocksExecAndNohupWrappers(t *testing.T) {
	cases := []string{
		"exec claude --print hi",
		"nohup claude &",
		"time claude",
		"command claude",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err)
		})
	}
}

func TestCanonicalName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude", "claude"},
		{"Claude", "claude"},
		{"claude.exe", "claude"},
		{"claude.CMD", "claude"},
		{"claude.bat", "claude"},
		{"/usr/local/bin/claude", "claude"},
		{`D:\bin\Claude.EXE`, "claude"},
		{"./claude", "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, canonicalName(tc.in))
		})
	}
}

func TestCheck_BlocksCommandWrappers(t *testing.T) {
	cases := []string{
		// cmd
		"start claude",
		`cmd /c "start claude --print hi"`,
		"start /b claude",
		// PowerShell cmdlets
		"Start-Process claude",
		"Start-Process -FilePath claude",
		`powershell -c "Start-Process claude"`,
		"Start-Job claude",
		// iex / Invoke-Expression
		`iex 'claude'`,
		`Invoke-Expression "claude -p test"`,
		`powershell -c "iex 'claude'"`,
		// PowerShell & invocation operator
		`powershell -c "& 'C:\Tools\claude.exe' -p hi"`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err, "should block: %s", cmd)
		})
	}
}

func TestCheck_BlocksEncodedCommand(t *testing.T) {
	// Encode "claude -p test" as UTF-16LE → base64 (PowerShell convention).
	// Verified output: c2EgIIA= is wrong — let's compute properly.
	// "claude" in UTF-16LE bytes: c\0 l\0 a\0 u\0 d\0 e\0
	// We do it programmatically in the test so it stays correct.
	src := "claude"
	u16 := make([]byte, 0, len(src)*2)
	for _, r := range src {
		u16 = append(u16, byte(r), 0)
	}
	encoded := base64.StdEncoding.EncodeToString(u16)
	cmd := "powershell -EncodedCommand " + encoded
	err := Check(cmd)
	require.Error(t, err, "encoded command containing 'claude' must be decoded and blocked: %s", cmd)
}

func TestIsEnvAssignment(t *testing.T) {
	assert.True(t, isEnvAssignment("FOO=bar"))
	assert.True(t, isEnvAssignment("F_OO123=value"))
	assert.False(t, isEnvAssignment("--flag=value"))
	assert.False(t, isEnvAssignment("=value"))
	assert.False(t, isEnvAssignment("no-equals"))
	assert.False(t, isEnvAssignment("123=bad"))
}

// --- CheckWindowSafety ------------------------------------------------------
//
// Separate concern from Check/commandWrappers above: Check asks "does this
// launch a denied AI agent", CheckWindowSafety asks "does this explicitly
// open a new, visible window that platform.Command's HideWindow on the
// OUTER process cannot suppress" — see the doc comment on
// WindowOpenerError for the mechanism. A harmless target (`start notepad`)
// must still be caught; that's the whole point.

func TestCheckWindowSafety_AllowsHarmless(t *testing.T) {
	for _, cmd := range []string{
		"",
		"ls -la",
		"git status",
		"echo hello",
		"go build ./...",
		`cmd /c "echo hi"`,
		"bash -c 'go build .'",
		// Similarly-named but NOT the flagged verb — must not false-positive
		// on a substring/prefix match.
		"restart-service foo",
		"Start-Sleep -Seconds 1",
		"startup-check.sh",
	} {
		t.Run(cmd, func(t *testing.T) {
			assert.Nil(t, CheckWindowSafety(cmd))
		})
	}
}

func TestCheckWindowSafety_BlocksDirectStartFamily(t *testing.T) {
	cases := []string{
		"start notepad.exe",
		"start /b notepad.exe",
		"Start-Process notepad.exe",
		"Start-Process -FilePath notepad.exe",
		"Start-Job notepad.exe",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := CheckWindowSafety(cmd)
			require.Error(t, err, "should block: %s", cmd)
			assert.Contains(t, err.Error(), "new, visible window")
		})
	}
}

func TestCheckWindowSafety_BlocksThroughShellWrappers(t *testing.T) {
	cases := []string{
		`cmd /c "start notepad.exe"`,
		`powershell -c "Start-Process notepad.exe"`,
		`bash -c "echo hi && cmd /c start notepad.exe"`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			require.Error(t, CheckWindowSafety(cmd), "should block: %s", cmd)
		})
	}
}

func TestCheckWindowSafety_BlocksInsideChainedCommand(t *testing.T) {
	require.Error(t, CheckWindowSafety("go build ./... && start notepad.exe"))
	require.Error(t, CheckWindowSafety("start notepad.exe; echo done"))
	require.Error(t, CheckWindowSafety("echo hi | cat && start calc.exe"))
}

func TestCheckWindowSafety_BlocksEncodedCommand(t *testing.T) {
	src := "Start-Process notepad.exe"
	u16 := make([]byte, 0, len(src)*2)
	for _, r := range src {
		u16 = append(u16, byte(r), 0)
	}
	encoded := base64.StdEncoding.EncodeToString(u16)
	cmd := "powershell -EncodedCommand " + encoded
	err := CheckWindowSafety(cmd)
	require.Error(t, err, "encoded command containing Start-Process must be decoded and blocked: %s", cmd)
}

func TestCheckWindowSafety_DoesNotBlockInvokeExpression(t *testing.T) {
	// iex/Invoke-Expression evaluate a string in the CURRENT shell — they
	// don't themselves request a new window (unlike start/Start-Process/
	// Start-Job). Out of scope for this check unless the evaluated string
	// itself contains a start-family verb.
	assert.Nil(t, CheckWindowSafety(`iex 'echo hi'`))
	assert.Nil(t, CheckWindowSafety(`Invoke-Expression "go build ./..."`))
}

func TestCheckWindowSafety_ErrorReportsVerbAndSnippet(t *testing.T) {
	err := CheckWindowSafety("start notepad.exe")
	require.Error(t, err)
	var winErr *WindowOpenerError
	require.ErrorAs(t, err, &winErr)
	assert.Equal(t, "start", winErr.Verb)
	assert.Equal(t, "start notepad.exe", winErr.Snippet)
}
