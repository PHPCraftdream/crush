// Package agentguard refuses bash invocations that would launch another AI
// coding agent from inside a crush sub-agent's tool surface. This closes
// the silent-recursion path where:
//
//   parent agent → crush run → sub-agent → bash → claude/codex/gemini → …
//
// — every link adds latency, multiplies token spend, and routinely
// times out before the deepest agent ever returns a useful answer.
// Architecturally a sub-agent should EXECUTE work, not re-delegate it.
// If the operator genuinely needs nested agents they invoke them directly
// in their own shell, where there is a human to confirm and watch costs.
//
// Fork patch: batch 16 — added after we burned an evening watching three
// nested crush invocations bake each other while doing zero real work.
package agentguard

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// DeniedError is returned by Check when a command is blocked. It is
// distinguished by type so callers can render a tool-failure result
// instead of treating it as an internal error.
type DeniedError struct {
	Tool    string // the matched agent name as the user typed it
	Reason  string // human-readable explanation
	Snippet string // the offending token / sub-command for forensic context
}

func (e *DeniedError) Error() string {
	if e.Snippet != "" {
		return fmt.Sprintf("agentguard: refused invocation of %q — %s (in: %s)", e.Tool, e.Reason, e.Snippet)
	}
	return fmt.Sprintf("agentguard: refused invocation of %q — %s", e.Tool, e.Reason)
}

// deniedAgents is the canonical denylist of AI-agent CLI binaries.
// Match is case-insensitive and considers .exe/.cmd/.bat/.ps1 suffixes.
// Names that overlap with common shell utilities (e.g. "continue" is also
// a shell keyword) — we accept the false-positive risk because the typical
// agent script does not use bare "continue" as a standalone command.
var deniedAgents = map[string]string{
	// Tier 1 — proprietary heavyweight
	"claude":       "Anthropic Claude Code",
	"codex":        "OpenAI Codex CLI",
	"gemini":       "Google Gemini CLI",
	"qwen":         "Alibaba Qwen Code",
	"qwen-code":    "Alibaba Qwen Code",
	"cody":         "Sourcegraph Cody CLI",
	"windsurf":     "Codeium Windsurf CLI",
	"windsurf-cli": "Codeium Windsurf CLI",

	// Tier 2 — open-source coding agents
	"opencode":     "opencode-ai",
	"aider":        "aider chat",
	"cline":        "Cline (Claude Dev)",
	"cursor-agent": "Cursor Agent CLI",
	"continue":     "Continue.dev CLI",
	"amp":          "Sourcegraph Amp",
	"amp-code":     "Sourcegraph Amp",
	"goose":        "Block Goose",
	"mentat":       "Mentat agent",
	"forge":        "Forge agent",
	"tabby":        "Tabby agent",

	// Tier 3 — us. Blocks any recursive crush invocation regardless of flags.
	"crush": "this very binary — recursive invocation is never the right answer",
}

// deniedNpmPackages lists packages a sub-agent might launch through npx /
// pnpm dlx / yarn dlx / bunx without the agent binary being on PATH.
var deniedNpmPackages = map[string]string{
	"@anthropic-ai/claude-code":    "Anthropic Claude Code (via npx)",
	"@openai/codex":                "OpenAI Codex CLI (via npx)",
	"@google/gemini-cli":           "Google Gemini CLI (via npx)",
	"@opencode-ai/opencode":        "opencode (via npx)",
	"@continue/cli":                "Continue.dev (via npx)",
	"@sourcegraph/amp":             "Sourcegraph Amp (via npx)",
	"@sourcegraph/cody":            "Sourcegraph Cody (via npx)",
	"@cursor-agent/cli":            "Cursor Agent (via npx)",
	"@windsurf/cli":                "Windsurf (via npx)",
	"@qwen-ai/qwen-cli":            "Qwen (via npx)",
}

// deniedPypiPackages: pipx / uvx wrappers.
var deniedPypiPackages = map[string]string{
	"aider-chat":  "aider (via pipx/uvx)",
	"aider-cli":   "aider (via pipx/uvx)",
	"mentat-cli":  "mentat (via pipx/uvx)",
}

// runners we look INTO — these wrap another command we must re-check.
var packageRunners = map[string]bool{
	"npx":   true,
	"pnpm":  true, // pnpm dlx X
	"yarn":  true, // yarn dlx X
	"bunx":  true,
	"bun":   true, // bun x X
	"pipx":  true, // pipx run X
	"uvx":   true,
	"uv":    true, // uv tool run X
}

var shellRunners = map[string]bool{
	"bash":          true,
	"sh":            true,
	"dash":          true,
	"zsh":           true,
	"ksh":           true,
	"fish":          true,
	"cmd":           true,
	"cmd.exe":       true,
	"powershell":    true,
	"powershell.exe": true,
	"pwsh":          true,
	"pwsh.exe":      true,
	"nu":            true, // nushell
}

// commandWrappers are commands that take ANOTHER command as their first
// non-flag argument and execute it. We strip them and re-check what they
// were going to launch. Without this `start claude` / `Start-Process claude`
// / `iex 'claude'` would bypass the denylist.
var commandWrappers = map[string]bool{
	"start":             true, // cmd: start <cmd> [args]
	"start-process":     true, // PowerShell cmdlet
	"start-job":         true, // PowerShell — runs in background but still launches the agent
	"invoke-expression": true, // PowerShell: invoke-expression "<string>"
	"iex":               true, // PowerShell alias for invoke-expression
	"invoke-command":    true, // PowerShell remote/local exec
	"icm":               true, // PowerShell alias for invoke-command
}

// Check inspects a shell command string and returns *DeniedError if it
// would launch a denied agent. nil means the command is allowed.
//
// It splits on ;, &&, ||, | first (so a denied agent buried in a pipeline
// is still caught), and for each segment walks the tokens. Shell wrappers
// (bash -c "X") and package runners (npx X) are recursed into one level.
func Check(command string) error {
	if command == "" {
		return nil
	}
	for _, segment := range splitChained(command) {
		if err := checkSegment(segment); err != nil {
			return err
		}
	}
	return nil
}

func checkSegment(segment string) error {
	tokens := tokenize(segment)
	if len(tokens) == 0 {
		return nil
	}
	// Skip leading env-var assignments (VAR=value VAR2=value cmd ...).
	i := 0
	for i < len(tokens) && strings.Contains(tokens[i], "=") && !strings.HasPrefix(tokens[i], "-") {
		// Heuristic: must look like an identifier=value, not --flag=value
		if isEnvAssignment(tokens[i]) {
			i++
		} else {
			break
		}
	}
	if i >= len(tokens) {
		return nil
	}
	head := tokens[i]
	rest := tokens[i+1:]

	// Strip leading "exec" / "command" / "time" wrappers (best effort).
	for (head == "exec" || head == "command" || head == "time" || head == "nohup") && len(rest) > 0 {
		head = rest[0]
		rest = rest[1:]
	}
	// PowerShell call operator: `& <command>` — strip the `&` so the actual
	// command lands in `head`.
	for head == "&" && len(rest) > 0 {
		head = rest[0]
		rest = rest[1:]
	}

	// Strip path + extension.
	headCanon := canonicalName(head)

	// Direct denied agent?
	if reason, ok := deniedAgents[headCanon]; ok {
		return &DeniedError{
			Tool:    head,
			Reason:  "AI agent CLI invocation is blocked by crush's architecture (would recurse / multiply cost). Tool: " + reason,
			Snippet: segment,
		}
	}

	// Shell runner: ... -c "X" — re-check X.
	if shellRunners[headCanon] {
		if inner := extractShellInner(headCanon, rest); inner != "" {
			if err := Check(inner); err != nil {
				return err
			}
		}
		return nil
	}

	// Command wrapper (start / Start-Process / iex / Invoke-Expression …):
	// first non-flag arg is the actual command we'd otherwise launch.
	// Strip leading PS-style argv (`&`, quoted launcher) and recurse.
	if commandWrappers[strings.ToLower(headCanon)] {
		if inner := extractWrapperInner(rest); inner != "" {
			if err := Check(inner); err != nil {
				return err
			}
		}
		return nil
	}

	// Package runner: npx <pkg> [args...], pnpm dlx <pkg>, yarn dlx <pkg>,
	// bun x <pkg>, pipx run <pkg>, uv tool run <pkg>.
	if packageRunners[headCanon] {
		if pkg := extractPackageRunnerTarget(headCanon, rest); pkg != "" {
			canon := strings.ToLower(pkg)
			if reason, ok := deniedNpmPackages[canon]; ok {
				return &DeniedError{Tool: pkg, Reason: reason + " — blocked", Snippet: segment}
			}
			if reason, ok := deniedPypiPackages[canon]; ok {
				return &DeniedError{Tool: pkg, Reason: reason + " — blocked", Snippet: segment}
			}
			// Also catch "npx claude" where someone aliased a package to
			// a denied binary name.
			if reason, ok := deniedAgents[canon]; ok {
				return &DeniedError{Tool: pkg, Reason: reason + " (via package runner) — blocked", Snippet: segment}
			}
		}
		return nil
	}

	return nil
}

// canonicalName strips directory prefix and a known executable suffix,
// then lower-cases. "/usr/bin/Claude.EXE" → "claude".
func canonicalName(name string) string {
	base := filepath.Base(name)
	low := strings.ToLower(base)
	for _, suf := range []string{".exe", ".cmd", ".bat", ".ps1", ".sh", ".py"} {
		if strings.HasSuffix(low, suf) {
			low = strings.TrimSuffix(low, suf)
			break
		}
	}
	return low
}

func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	name := tok[:eq]
	// POSIX: env var names cannot start with a digit.
	if name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, r := range name {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// splitChained breaks the command on top-level &&, ||, ;, |. Naive — does
// not understand subshells or quoted operators. For our denial purposes
// false-positive splits inside quotes are harmless (we just check more
// segments than strictly necessary).
func splitChained(s string) []string {
	out := []string{}
	cur := strings.Builder{}
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			cur.WriteByte(c)
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			cur.WriteByte(c)
		case ';':
			if !inSingle && !inDouble {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
			cur.WriteByte(c)
		case '|':
			if !inSingle && !inDouble {
				out = append(out, cur.String())
				cur.Reset()
				// skip second '|' in ||
				if i+1 < len(s) && s[i+1] == '|' {
					i++
				}
				continue
			}
			cur.WriteByte(c)
		case '&':
			if !inSingle && !inDouble && i+1 < len(s) && s[i+1] == '&' {
				out = append(out, cur.String())
				cur.Reset()
				i++
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// tokenize is a very small quote-aware splitter. Handles ' ' and " "
// quoting; everything else is whitespace.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case (c == ' ' || c == '\t' || c == '\n' || c == '\r') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// extractShellInner reads -c / /c / -Command argument from a shell wrapper.
// For -EncodedCommand the base64 payload is decoded (UTF-16LE per
// PowerShell's convention) before being returned for re-checking.
func extractShellInner(shell string, rest []string) string {
	for i, t := range rest {
		switch shell {
		case "cmd", "cmd.exe":
			// cmd /c "..."  or  cmd /k "..."
			if (strings.EqualFold(t, "/c") || strings.EqualFold(t, "/k")) && i+1 < len(rest) {
				return strings.Join(rest[i+1:], " ")
			}
		case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
			// -EncodedCommand <base64-utf16le>: decode, then recurse.
			if strings.EqualFold(t, "-encodedcommand") || strings.EqualFold(t, "-enc") || strings.EqualFold(t, "-e") {
				if i+1 < len(rest) {
					if decoded := decodePowerShellEncoded(rest[i+1]); decoded != "" {
						return decoded
					}
				}
				continue
			}
			if (strings.EqualFold(t, "-c") || strings.EqualFold(t, "-command")) && i+1 < len(rest) {
				return strings.Join(rest[i+1:], " ")
			}
		default: // bash / sh / dash / zsh / ksh / fish / nu
			if t == "-c" && i+1 < len(rest) {
				return rest[i+1]
			}
		}
	}
	return ""
}

// decodePowerShellEncoded decodes the base64 payload of
// `powershell -EncodedCommand <b64>`. PowerShell expects the input to be
// UTF-16LE encoded BEFORE base64. Returns "" if anything goes wrong (we
// then fall through to allowing the segment — safer than crashing).
func decodePowerShellEncoded(b64 string) string {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	if len(raw)%2 != 0 {
		return ""
	}
	u16 := make([]uint16, len(raw)/2)
	for i := 0; i < len(u16); i++ {
		u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return string(utf16.Decode(u16))
}

// extractWrapperInner pulls out the actual command from a wrapper-style
// invocation (`start <cmd>`, `Start-Process <cmd>`, `iex "<cmd>"` …).
// Skips leading PowerShell-isms — quoted strings, the `&` invocation
// operator, and -Verb/-WindowStyle/-FilePath flag spellings.
func extractWrapperInner(rest []string) string {
	for i, t := range rest {
		if t == "" {
			continue
		}
		// PowerShell flags often paired with Start-Process: -FilePath <cmd>,
		// -ArgumentList "..."; the actual exe sits behind -FilePath.
		if strings.EqualFold(t, "-filepath") && i+1 < len(rest) {
			return rest[i+1]
		}
		// Skip POSIX-style (-x) AND cmd-style (/x) flags. `start /b claude`,
		// `start /min claude`, etc.
		if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "/") || t == "--" {
			continue
		}
		// PowerShell call operator: & "<command>" or & 'command'
		if t == "&" {
			continue
		}
		return strings.Trim(t, `"'`)
	}
	return ""
}

// extractPackageRunnerTarget returns the FIRST positional arg that is the
// package name. Skips runner-specific flags (-y, --yes, --) and the "dlx"
// / "x" / "tool run" sub-commands of pnpm / yarn / bun / uv.
func extractPackageRunnerTarget(runner string, rest []string) string {
	skip := map[string]bool{}
	switch runner {
	case "pnpm", "yarn":
		skip["dlx"] = true
	case "bun":
		skip["x"] = true
	case "uv":
		skip["tool"] = true
		skip["run"] = true
	case "pipx":
		skip["run"] = true
	}
	for _, t := range rest {
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "-") || t == "--" {
			continue
		}
		if skip[t] {
			continue
		}
		return t
	}
	return ""
}
