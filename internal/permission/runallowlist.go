package permission

// This file implements the restricted-run allowlist: the matcher that
// decides whether a `crush run` permission request is auto-approved
// when restricted mode is on. See config.RunPermissions for the
// user-facing config and cmd/run.go for the --restrict-run / --allow-bash
// CLI surface.
//
// Design notes:
//
//  1. No ad-hoc shell splitting. Command patterns are matched against
//     the whole command string with well-defined semantics (exact,
//     word-boundary prefix, cross-platform glob, or regexp). The
//     chaining guard parses the command with the same shell grammar the
//     bash tool executes (mvdan.cc/sh/v3) so a prefix/exact pattern can
//     never authorise a compound command — see commandIsCompound.
//
//  2. The matcher is total: it never panics and never blocks. A pattern
//     that fails to compile (bad regex, bad glob) is reported once via
//     BuildRunAllowlist and then ignored at match time, so a single bad
//     pattern can't lock out an entire run.
//
//  3. Deny is the safe direction: an unrecognised params shape, an empty
//     command, or an unmatched request all fall through to "not allowed".

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"
)

// RunAllowlistSpec is the user-facing, pre-compilation form of a
// restricted-run allowlist. It mirrors config.RunPermissions and the
// `crush run` CLI flags; BuildRunAllowlist compiles it into a queryable
// RunAllowlist.
type RunAllowlistSpec struct {
	// Restrict enables the restricted permission model. When false the
	// allowlist is inert and `crush run` keeps auto-approving everything.
	Restrict bool
	// AllowTools lists "tool" and "tool:action" keys that bypass the
	// run gate for non-bash tools. Same syntax as permissions.allowed_tools.
	// NOTE: entries for "bash" / "bash:execute" are deliberately ignored
	// by the gate — bash is governed solely by AllowBash so an operator
	// can't accidentally authorise arbitrary shell commands by listing
	// the tool name. See allowsRequest.
	AllowTools []string
	// AllowBash lists bash command patterns. See RunAllowlistSpec doc
	// comment above and config.RunPermissions for the syntax.
	AllowBash []string
}

// bashPatternKind identifies how a compiled bash pattern matches.
type bashPatternKind int

const (
	bashPatternExact  bashPatternKind = iota // "exact:cmd" — whole-string match.
	bashPatternPrefix                        // "cmd args" — word-boundary prefix.
	bashPatternGlob                          // "glob:pat" — cross-platform glob (see globToRegexp).
	bashPatternRegex                         // "regex:pat" — regexp.MatchString.
)

// compiledBashPattern is a single parsed bash pattern. raw is retained
// for diagnostics; the matcher uses only the compiled fields.
type compiledBashPattern struct {
	raw   string
	kind  bashPatternKind
	value string         // exact / prefix / glob body
	re    *regexp.Regexp // regex body (compiled)
}

// RunAllowlist is the compiled, concurrency-safe, ready-to-query form
// of a restricted-run allowlist. The zero value is an inert (empty)
// allowlist; IsRestricted reports whether the gate is armed.
type RunAllowlist struct {
	restrict     bool
	allowTools   map[string]struct{} // "tool" and "tool:action" keys
	bashPatterns []compiledBashPattern
}

// IsRestricted reports whether restricted mode is armed. When false the
// matcher never denies — callers keep the legacy auto-approve behaviour.
func (a RunAllowlist) IsRestricted() bool { return a.restrict }

// allowsRequest reports whether opts is permitted by this allowlist.
// The caller MUST first check IsRestricted; calling this on a
// non-restricted allowlist is undefined for performance but safe.
//
// Semantics (conservative by design):
//
//   - Bash is governed ONLY by AllowBash command patterns. AllowTools
//     entries for "bash" or "bash:execute" do NOT bypass command
//     scrutiny — listing bash there is a silent no-op for the run gate.
//     This keeps the two surfaces non-overlapping: AllowTools scopes
//     which non-bash tools may run; AllowBash scopes which commands
//     bash may run. (The global permissions.allowed_tools fast-path
//     still wins over both because it is checked earlier in Request —
//     that is the documented operator escape hatch for a full bash
//     bypass, not this gate's concern.)
//   - Every other tool is approved iff it (or its tool:action) is in
//     the AllowTools table.
//   - Empty/unreadable bash commands are denied.
func (a RunAllowlist) allowsRequest(opts CreatePermissionRequest) bool {
	// Bash gets command-level scrutiny ONLY. We deliberately do not
	// consult the AllowTools table here, even if "bash" or
	// "bash:execute" is listed: a tool-name match must not authorise an
	// arbitrary shell command. Operators who want to approve bash
	// wholesale must use permissions.allowed_tools (the pre-gate
	// fast-path), not run.allow_tools.
	//
	// The literal "bash" must stay equal to internal/agent/tools.BashToolName
	// (we can't import it here without an import cycle). A cross-package
	// guard in internal/app (TestRunAllowlist_BashToolNameLiteralMatchesConstant)
	// fails the build if the tool is ever renamed and this literal is not.
	if opts.ToolName == "bash" {
		cmd := extractBashCommand(opts.Params)
		if cmd == "" {
			// Bash call with no inspectable command — deny. We refuse
			// to auto-approve an unknown shell command in restricted mode.
			return false
		}
		return bashCommandAllowed(a.bashPatterns, cmd)
	}
	return a.toolAllowed(opts.ToolName, opts.Action)
}

// toolAllowed reports whether the tool (or tool:action) is in the
// AllowTools table. It is consulted ONLY for non-bash tools; the bash
// branch of allowsRequest ignores it entirely (see the doc comment
// there). An empty table denies every non-bash tool.
func (a RunAllowlist) toolAllowed(toolName, action string) bool {
	if len(a.allowTools) == 0 {
		return false
	}
	if _, ok := a.allowTools[toolName]; ok {
		return true
	}
	if action != "" {
		if _, ok := a.allowTools[toolName+":"+action]; ok {
			return true
		}
	}
	return false
}

// BuildRunAllowlist compiles spec into a queryable RunAllowlist. A
// pattern that fails to compile (bad regex / bad glob) is returned as
// an error AND dropped from the result, so the caller can log it while
// still arming the allowlist with the remaining valid patterns.
func BuildRunAllowlist(spec RunAllowlistSpec) (RunAllowlist, error) {
	out := RunAllowlist{
		restrict:   spec.Restrict,
		allowTools: make(map[string]struct{}, len(spec.AllowTools)),
	}
	for _, t := range spec.AllowTools {
		t = strings.TrimSpace(t)
		if t != "" {
			out.allowTools[t] = struct{}{}
		}
	}

	var firstErr error
	for _, raw := range spec.AllowBash {
		compiled, err := compileBashPattern(raw)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out.bashPatterns = append(out.bashPatterns, compiled)
	}
	return out, firstErr
}

// MergeRunAllowlistSpecs unions two specs into one. Used to combine the
// config-derived allowlist with the per-invocation CLI overrides. The
// result restricts if either side restricts; tool and bash lists are
// concatenated (dedup is applied at compile time for tools and is not
// needed for bash patterns — duplicates just match twice).
func MergeRunAllowlistSpecs(a, b RunAllowlistSpec) RunAllowlistSpec {
	merged := RunAllowlistSpec{
		Restrict:   a.Restrict || b.Restrict,
		AllowTools: append([]string{}, a.AllowTools...),
		AllowBash:  append([]string{}, a.AllowBash...),
	}
	merged.AllowTools = append(merged.AllowTools, b.AllowTools...)
	merged.AllowBash = append(merged.AllowBash, b.AllowBash...)
	return merged
}

// compileBashPattern parses a single AllowBash entry. Recognised forms:
//
//	"regex:pat"  → regexp
//	"glob:pat"   → cross-platform glob (compiled to an anchored regexp)
//	"exact:cmd"  → whole-string equality after TrimSpace
//	anything else → word-boundary prefix (the common case, e.g. "git diff")
//
// The prefix, exact, and glob forms are compound-guarded at match time,
// not at compile time — the pattern itself is always valid even if it
// would never match a compound command. Only regex is exempt from the
// compound guard.
func compileBashPattern(raw string) (compiledBashPattern, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return compiledBashPattern{}, errEmptyBashPattern
	}

	switch {
	case strings.HasPrefix(raw, "regex:"):
		body := strings.TrimPrefix(raw, "regex:")
		if body == "" {
			return compiledBashPattern{}, errEmptyPatternBody(raw)
		}
		re, err := regexp.Compile(body)
		if err != nil {
			return compiledBashPattern{}, errBadPattern(raw, err)
		}
		return compiledBashPattern{raw: raw, kind: bashPatternRegex, re: re}, nil

	case strings.HasPrefix(raw, "glob:"):
		body := strings.TrimPrefix(raw, "glob:")
		if body == "" {
			return compiledBashPattern{}, errEmptyPatternBody(raw)
		}
		// Compile the glob to an anchored regexp so matching is
		// cross-platform (see globToRegexp — filepath.Match's `*` was
		// separator-aware, which made a glob behave differently on
		// Windows vs Linux).
		re, err := globToRegexp(body)
		if err != nil {
			return compiledBashPattern{}, errBadPattern(raw, err)
		}
		return compiledBashPattern{raw: raw, kind: bashPatternGlob, value: body, re: re}, nil

	case strings.HasPrefix(raw, "exact:"):
		body := strings.TrimSpace(strings.TrimPrefix(raw, "exact:"))
		if body == "" {
			return compiledBashPattern{}, errEmptyPatternBody(raw)
		}
		return compiledBashPattern{raw: raw, kind: bashPatternExact, value: body}, nil

	default:
		// Prefix form: the body is the raw entry verbatim (already
		// trimmed). An empty body was rejected above.
		return compiledBashPattern{raw: raw, kind: bashPatternPrefix, value: raw}, nil
	}
}

// globToRegexp translates a shell-style glob (only `*` and `?` are
// special) into an anchored regexp matching the WHOLE command string.
// Unlike filepath.Match, `*` matches any run of characters INCLUDING
// `/`, so a glob behaves identically on every OS — filepath.Match's
// separator-aware `*` made "glob:ls *" match "ls /etc" on Windows but
// not on Linux, an OS-dependent authorization decision. Every other
// regex metacharacter is escaped and matched literally.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteByte('^')
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}

// commandIsCompound reports whether cmd is anything other than a single
// simple command — i.e. it chains (`;`, a raw newline, `&&`, `||`, `|`),
// backgrounds (`&`), substitutes (`$(...)`, backticks), or opens a
// subshell / block / control structure. Prefix and exact patterns refuse
// to match compound commands so a permissive prefix such as "ls" cannot
// authorise "ls && rm -rf /" — or "ls\nrm -rf /", "ls & rm -rf /".
//
// The command is parsed with the same shell grammar the bash tool
// actually executes (mvdan.cc/sh/v3), rather than scanning for a fixed
// set of metacharacter substrings. This closes two gaps in the old
// substring scan — a raw newline and a bare backgrounding `&` were not
// in the list — while, unlike a substring scan, NOT tripping on a
// metacharacter that appears inside quotes (`echo 'a; b'` is a single
// command) or in a plain redirection (`ls 2>&1` operates on one
// command). A command that fails to parse is treated as compound: we
// refuse to auto-approve something we can't prove is a single command.
func commandIsCompound(cmd string) bool {
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
	// Anything that isn't a single simple command (a pipeline/`&&`/`||`
	// BinaryCmd, a subshell, a block, an if/for/while, …) is compound.
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

// bashCommandAllowed reports whether cmd satisfies any of the compiled
// patterns. An empty pattern list denies everything (restricted mode is
// deny-by-default).
func bashCommandAllowed(patterns []compiledBashPattern, cmd string) bool {
	command := strings.TrimSpace(cmd)
	if command == "" {
		return false
	}
	compound := commandIsCompound(command)
	for _, p := range patterns {
		switch p.kind {
		case bashPatternPrefix:
			if compound {
				continue
			}
			if prefixWordBoundary(p.value, command) {
				return true
			}
		case bashPatternExact:
			if compound {
				continue
			}
			if p.value == command {
				return true
			}
		case bashPatternGlob:
			// A glob is a convenience form, so — like prefix/exact — it
			// must not authorise a compound command (`glob:ls *` cannot
			// approve "ls && rm -rf /"). Operators who genuinely need to
			// match a compound command must use an explicit regex.
			if compound {
				continue
			}
			if p.re != nil && p.re.MatchString(command) {
				return true
			}
		case bashPatternRegex:
			if p.re != nil && p.re.MatchString(command) {
				return true
			}
		}
	}
	return false
}

// prefixWordBoundary reports whether command begins with pattern such
// that the byte immediately after the pattern is a word boundary: end
// of string or ASCII whitespace. This prevents "git" from matching
// "gittools" while still matching "git diff HEAD".
//
// Matching is case-sensitive for predictability — user-provided
// patterns are matched verbatim, which is the least surprising choice
// across macOS (case-insensitive HFS+) and Linux (case-sensitive ext4).
func prefixWordBoundary(pattern, command string) bool {
	if pattern == "" {
		return false
	}
	if !strings.HasPrefix(command, pattern) {
		return false
	}
	if len(command) == len(pattern) {
		return true
	}
	next := command[len(pattern)]
	return next == ' ' || next == '\t' || next == '\n'
}

type runAllowlistCommandProvider interface {
	RunAllowlistCommand() string
}

// extractBashCommand reads the bash command from permission params. The
// real bash params type implements runAllowlistCommandProvider; the
// reflection fallback keeps tests and older mirror structs working
// without importing internal/agent/tools and creating a package cycle.
// Returns "" when params is nil, non-struct, or has no command.
func extractBashCommand(params any) string {
	if params == nil {
		return ""
	}
	if provider, ok := params.(runAllowlistCommandProvider); ok {
		return provider.RunAllowlistCommand()
	}
	v := reflect.ValueOf(params)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	f := v.FieldByName("Command")
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}

// runAllowlistGate wraps a RunAllowlist with the mutex it shares with
// the permission service. The service embeds this so SetRunAllowlist
// (writer) and the Request path (reader) stay race-free.
type runAllowlistGate struct {
	mu       sync.RWMutex
	compiled RunAllowlist
}

func (g *runAllowlistGate) load() RunAllowlist {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.compiled
}

func (g *runAllowlistGate) store(a RunAllowlist) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.compiled = a
}

// Pattern-matching error sentinels. Kept unexported and wrapped via
// errBadPattern / errEmptyPatternBody so callers get the offending
// pattern in the message.

var errEmptyBashPattern = patternError("empty bash allow pattern")

type patternError string

func (e patternError) Error() string { return string(e) }

func errEmptyPatternBody(raw string) error {
	return patternError("empty pattern body in " + strconv.Quote(raw))
}

func errBadPattern(raw string, cause error) error {
	return patternError("invalid pattern " + strconv.Quote(raw) + ": " + cause.Error())
}
