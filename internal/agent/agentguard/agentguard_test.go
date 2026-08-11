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

// B-2 regression test (found by a full-project @crush --role reviewer
// audit, 2026-08-11): env/nice/timeout were missing from the wrapper
// strip-list, so "env claude ...", "nice claude ...", "timeout 30 claude
// ..." all bypassed this guard entirely — head stayed "env"/"nice"/
// "timeout", never matching deniedAgents. Doubly dangerous in combination
// with the sibling B-1 bug (these same three names were also in
// tools/safe.go's safeCommands, skipping the permission prompt too), so a
// single "env claude --dangerously-skip-permissions ..." bypassed both the
// recursion guard and the permission prompt at once.
//
// REVERT CHECK PROCEDURE:
//  1. In agentguard.go's checkSegment, remove the "env", "nice", "timeout"
//     cases from the stripLoop switch (restore the original 4-case bare
//     `for (head == "exec" || ...) ...` loop).
//  2. Run: go test ./internal/agent/agentguard -run TestCheck_BlocksEnvNiceTimeoutWrappers -v
//  3. FAIL: none of these commands are blocked.
//  4. Restore the strip cases and PASS.
func TestCheck_BlocksEnvNiceTimeoutWrappers(t *testing.T) {
	cases := []string{
		"env claude -p test",
		"env -i claude -p test",
		"env FOO=bar BAZ=qux claude -p test",
		"env -u PATH claude -p test",
		"nice claude -p test",
		"nice -n 10 claude -p test",
		"timeout 30 claude -p test",
		"timeout -k 5 30 claude -p test",
		"timeout --signal=KILL 30 claude -p test",
		// Stacked wrappers: env wrapping nohup wrapping the real agent.
		"env nohup claude -p test",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err, "must block the agent invocation wrapped by env/nice/timeout")
			var de *DeniedError
			require.True(t, errors.As(err, &de))
			assert.Equal(t, "claude", de.Tool)
		})
	}
}

// TestCheck_EnvNiceTimeoutAllowHarmlessCommands proves the fix didn't
// overcorrect: env/nice/timeout wrapping an ordinary, non-agent command are
// still allowed.
func TestCheck_EnvNiceTimeoutAllowHarmlessCommands(t *testing.T) {
	cases := []string{
		"env echo hi",
		"env FOO=bar npm test",
		"nice -n 10 npm run build",
		"timeout 30 npm test",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			assert.NoError(t, Check(cmd))
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

// P2-2 regression tests (2026-08-11 release readiness review):
// Part 1: env -S/--split-string bypass - GNU env's -S flag parses its
// value as a command string and executes it. Without recursive checking,
// "env -S 'claude -p test'" bypasses the guard entirely.
//
// REVERT CHECK PROCEDURE:
//  1. Comment out the split-string recursion loop in checkSegment
//  2. Run: go test ./internal/agent/agentguard -run TestCheck_BlocksEnvSplitStringBypass -v
//  3. FAIL: all these commands return nil instead of *DeniedError
//  4. Restore the loop and PASS.
func TestCheck_BlocksEnvSplitStringBypass(t *testing.T) {
	cases := []string{
		// -S with space-separated value
		"env -S 'claude -p test'",
		"env -S \"claude -p test\"",
		// --split-string with space-separated value
		"env --split-string 'claude -p test'",
		"env --split-string \"claude -p test\"",
		// --split-string=VALUE form (GNU long option with =)
		"env --split-string='claude -p test'",
		"env --split-string=\"claude -p test\"",
		// Multiple split-strings - first one triggers block
		"env -S 'echo hello' -S 'claude -p test' echo ignored",
		// Combined with other env flags
		"env -i -S 'claude -p test'",
		"env FOO=bar -S 'claude -p test'",
		// Stacked with other wrappers
		"nice -n 10 env -S 'claude -p test'",
		"timeout 30 env -S 'claude -p test'",
		// Package runner inside split-string (must be caught after recursion)
		"env -S 'npx @anthropic-ai/claude-code'",
		// Shell wrapper inside split-string (must be caught after recursion)
		"env -S 'bash -c claude'",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := Check(cmd)
			require.Error(t, err, "must block agent invocation via env -S: %s", cmd)
			var de *DeniedError
			require.True(t, errors.As(err, &de), "expected DeniedError, got %T for: %s", err, cmd)
			assert.Contains(t, strings.ToLower(de.Error()), "blocked")
		})
	}
}

// Part 2: checkSegmentWindowSafety missing env/nice/timeout wrappers.
// Before the fix, checkSegmentWindowSafety had its own hardcoded list
// of wrappers (only exec/command/time/nohup + &) that didn't include
// env/nice/timeout. This meant "env start notepad" bypassed the
// window-safety check because head never reached "start".
//
// REVERT CHECK PROCEDURE:
//  1. In checkSegmentWindowSafety, restore the old manual wrapper loop
//     (before resolveCommandHead refactoring):
//     for (head == "exec" || head == "command" || head == "time" || head == "nohup") ...
//  2. Run: go test ./internal/agent/agentguard -run TestCheckWindowSafety_BlocksEnvNiceTimeoutWrappers -v
//  3. FAIL: env/nice/timeout variants return nil instead of *WindowOpenerError
//  4. Restore resolveCommandHead usage and PASS.
func TestCheckWindowSafety_BlocksEnvNiceTimeoutWrappers(t *testing.T) {
	cases := []struct {
		cmd      string
		expected string // the verb we expect in the error
	}{
		// env wrapping start
		{"env start notepad.exe", "start"},
		{"env -i start notepad.exe", "start"},
		{"env FOO=bar Start-Process notepad.exe", "start-process"},
		// nice wrapping start
		{"nice -n 10 start notepad.exe", "start"},
		{"nice Start-Process notepad.exe", "start-process"},
		// timeout wrapping start
		{"timeout 30 start notepad.exe", "start"},
		{"timeout --signal=KILL 30 Start-Process notepad.exe", "start-process"},
		// Stacked: env wrapping nice wrapping start
		{"env nice -n 10 start notepad.exe", "start"},
		// Through shell wrapper too (double recursion)
		{`bash -c "env start notepad.exe"`, "start"},
		{`powershell -c "env start notepad.exe"`, "start"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			err := CheckWindowSafety(tc.cmd)
			require.Error(t, err, "must block window opener via env/nice/timeout wrapper: %s", tc.cmd)
			var winErr *WindowOpenerError
			require.ErrorAs(t, err, &winErr, "expected WindowOpenerError, got %T for: %s", err, tc.cmd)
			assert.Equal(t, tc.expected, winErr.Verb)
		})
	}
}

// Test that split-string recursion works in CheckWindowSafety too.
// "env -S 'start notepad'" should be caught because the -S payload
// gets recursively checked.
func TestCheckWindowSafety_BlocksEnvSplitStringWithWindowOpener(t *testing.T) {
	cases := []string{
		"env -S 'start notepad.exe'",
		"env --split-string='Start-Process notepad.exe'",
		"env -S \"Start-Job notepad.exe\"",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			err := CheckWindowSafety(cmd)
			require.Error(t, err, "must block window opener via env -S recursion: %s", cmd)
			var winErr *WindowOpenerError
			require.ErrorAs(t, err, &winErr)
		})
	}
}

// Test that env -S with harmless content is allowed (no false positives).
func TestCheck_EnvSplitStringAllowsHarmless(t *testing.T) {
	cases := []string{
		"env -S 'echo hello'",
		"env -S 'git status'",
		"env -S 'go build ./...'",
		"env --split-string='ls -la'",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			assert.NoError(t, Check(cmd))
		})
	}
}

// Test that env/nice/timeout wrapping harmless commands is still allowed.
func TestCheckWindowSafety_EnvNiceTimeoutAllowsHarmlessCommands(t *testing.T) {
	cases := []string{
		"env echo hi",
		"env FOO=bar npm test",
		"nice -n 10 npm run build",
		"timeout 30 npm test",
		// With window-safe commands inside
		"env git status",
		"nice ls -la",
		"timeout 10 echo done",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			assert.Nil(t, CheckWindowSafety(cmd))
		})
	}
}
