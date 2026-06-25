package cmd

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatToolCallPreview(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"bash with command", "bash", `{"command":"go test ./..."}`, "go test ./..."},
		{"bash long command truncated", "bash", `{"command":"` + strings.Repeat("x", 200) + `"}`, strings.Repeat("x", 79) + "…"},
		{"view with file_path only", "view", `{"file_path":"main.go"}`, "main.go"},
		{"view with offset+limit", "view", `{"file_path":"main.go","offset":100,"limit":50}`, "main.go [100:+50]"},
		{"view with limit only", "view", `{"file_path":"main.go","limit":200}`, "main.go [:200]"},
		{"edit with file_path", "edit", `{"file_path":"a.go","old_string":"x","new_string":"y"}`, "a.go"},
		{"multiedit with file_path", "multiedit", `{"file_path":"b.go","edits":[]}`, "b.go"},
		{"write with file_path", "write", `{"file_path":"c.go","content":"package main"}`, "c.go"},
		{"grep pattern + path", "grep", `{"pattern":"func main","path":"internal/"}`, `"func main" in internal/`},
		{"grep pattern only", "grep", `{"pattern":"TODO"}`, `"TODO"`},
		{"glob pattern", "glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"ls path", "ls", `{"path":"internal/cmd"}`, "internal/cmd"},
		{"fetch url", "fetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"web_fetch url", "web_fetch", `{"url":"https://example.com/x"}`, "https://example.com/x"},
		{"download url+dst", "download", `{"url":"https://x.com/y","file_path":"/tmp/y"}`, "https://x.com/y → /tmp/y"},
		{"sourcegraph query", "sourcegraph", `{"query":"repo:foo lang:go"}`, "repo:foo lang:go"},
		{"agent description", "agent", `{"description":"refactor login","prompt":"do it"}`, "refactor login"},
		{"agent prompt fallback", "agent", `{"prompt":"do it"}`, "do it"},
		{"task description", "task", `{"description":"audit ports","prompt":"long…"}`, "audit ports"},
		{"todowrite count", "todowrite", `{"todos":[{"id":1},{"id":2},{"id":3}]}`, "3 todos"},
		{"empty input", "bash", ``, ""},
		{"whitespace input", "bash", `   `, ""},
		{"invalid json fallback", "bash", `not-json{garbage`, "not-json{garbage"},
		{"unknown tool falls back to first string field", "weird", `{"alpha":"hello","beta":"world"}`, "alpha=hello"},
		{"unknown tool nothing string", "weird", `{"n":42,"b":true}`, ""},
		{"case insensitive tool name", "BASH", `{"command":"ls"}`, "ls"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatToolCallPreview(c.tool, c.input)
			assert.Equal(t, c.want, got, "tool=%s input=%q", c.tool, c.input)
		})
	}
}

func TestFormatToolResultPreview(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n\t\n  ", ""},
		{"short single line", "ok", "ok"},
		{"exact 200 chars single line", strings.Repeat("a", 200), strings.Repeat("a", 200)},
		{"long single line truncated", strings.Repeat("a", 300), strings.Repeat("a", 199) + "…"},
		{"multiline short first", "first line\nsecond\nthird", "first line (+2 lines)"},
		{"multiline first with leading whitespace", "   first\nsecond", "first (+1 lines)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Backward-compat path: no origin info → content-only preview.
			got := formatToolResultPreview(c.content, "", "")
			assert.Equal(t, c.want, got, "content=%q", c.content)
		})
	}
}

func TestFormatToolResultPreview_WithOriginHint(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		originName  string
		originInput string
		want        string
	}{
		// view / edit / multiedit / write → file_path as hint.
		{
			"view shows file_path before content",
			"package cmd\n\nimport (\n",
			"view", `{"file_path":"internal/cmd/sessions.go","limit":50}`,
			"internal/cmd/sessions.go: package cmd (+2 lines)",
		},
		{
			"view with empty content shows file_path alone",
			"",
			"view", `{"file_path":"internal/cmd/sessions.go"}`,
			"internal/cmd/sessions.go",
		},
		{
			"edit shows file_path with <result> body",
			"<result>\nedited\nfoo\nbar\n",
			"edit", `{"file_path":"a.go","old_string":"x","new_string":"y"}`,
			"a.go: <result> (+3 lines)",
		},
		{
			"multiedit shows file_path",
			"<result>",
			"multiedit", `{"file_path":"b.go","edits":[{}]}`,
			"b.go: <result>",
		},
		{
			"write shows file_path",
			"<wrote 42 bytes>",
			"write", `{"file_path":"c.go","content":"package main"}`,
			"c.go: <wrote 42 bytes>",
		},

		// grep / glob / ls → pattern / path.
		{
			"grep with both pattern+path",
			"main.go:42:TODO\nmain.go:88:TODO\n",
			"grep", `{"pattern":"TODO","path":"internal/"}`,
			`"TODO" in internal/: main.go:42:TODO (+1 lines)`,
		},
		{
			"grep pattern only",
			"a.go:1:func main",
			"grep", `{"pattern":"func main"}`,
			`"func main": a.go:1:func main`,
		},
		{
			"glob pattern",
			"a.go\nb.go\nc.go\n",
			"glob", `{"pattern":"**/*.go"}`,
			"**/*.go: a.go (+2 lines)",
		},
		{
			"ls path",
			"foo.txt\nbar.txt",
			"ls", `{"path":"/tmp"}`,
			"/tmp: foo.txt (+1 lines)",
		},

		// fetch / download → url.
		{
			"fetch shows url",
			"<html><body>...",
			"fetch", `{"url":"https://example.com"}`,
			"https://example.com: <html><body>...",
		},
		{
			"download shows destination over url",
			"",
			"download", `{"url":"https://x.com/y","file_path":"/tmp/y"}`,
			"/tmp/y",
		},
		{
			"download falls back to url when no file_path",
			"saved",
			"download", `{"url":"https://x.com/y"}`,
			"https://x.com/y: saved",
		},

		// bash / sourcegraph / agent → command/query/prompt hint + content,
		// so the result row says WHAT ran even when output is empty.
		{
			"bash shows command + output",
			"hello world",
			"bash", `{"command":"echo hello world"}`,
			"echo hello world: hello world",
		},
		{
			"bash empty output shows command only",
			"no output",
			"bash", `{"command":"git add -A"}`,
			"git add -A: no output",
		},
		{
			"sourcegraph shows query + matches",
			"match 1\nmatch 2",
			"sourcegraph", `{"query":"repo:foo lang:go"}`,
			"repo:foo lang:go: match 1 (+1 lines)",
		},
		{
			"agent shows description + content",
			"done",
			"agent", `{"description":"refactor login","prompt":"do it"}`,
			"refactor login: done",
		},
		{
			"todowrite stays content-only (no hint)",
			"Todo list updated successfully.",
			"todowrite", `{"todos":[]}`,
			"Todo list updated successfully.",
		},
		{
			"unknown tool falls back to content-only",
			"some content",
			"weirdtool", `{"alpha":"hello"}`,
			"some content",
		},

		// Robustness: bad JSON, missing fields, empty origin.
		{
			"unparseable origin input → content-only",
			"some content",
			"view", "not-json{",
			"some content",
		},
		{
			"empty origin input → content-only",
			"some content",
			"view", "",
			"some content",
		},
		{
			"view without file_path in input → content-only",
			"some content",
			"view", `{"offset":10}`,
			"some content",
		},
		{
			"both empty → empty string",
			"",
			"view", `{"file_path":""}`,
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatToolResultPreview(c.content, c.originName, c.originInput)
			assert.Equal(t, c.want, got, "content=%q origin=%s/%s", c.content, c.originName, c.originInput)
		})
	}
}

func TestToolResultOriginHint(t *testing.T) {
	// Direct unit test of the origin-hint helper so the per-tool branch
	// table stays covered even if formatToolResultPreview changes shape.
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"view", "view", `{"file_path":"a.go"}`, "a.go"},
		{"edit", "edit", `{"file_path":"a.go"}`, "a.go"},
		{"multiedit", "multiedit", `{"file_path":"a.go"}`, "a.go"},
		{"write", "write", `{"file_path":"a.go"}`, "a.go"},
		{"grep both", "grep", `{"pattern":"x","path":"src/"}`, `"x" in src/`},
		{"grep pat only", "grep", `{"pattern":"x"}`, `"x"`},
		{"grep path only", "grep", `{"path":"src/"}`, "src/"},
		{"glob", "glob", `{"pattern":"*.go"}`, "*.go"},
		{"ls", "ls", `{"path":"/tmp"}`, "/tmp"},
		{"fetch", "fetch", `{"url":"https://x.com"}`, "https://x.com"},
		{"web_fetch", "web_fetch", `{"url":"https://x.com"}`, "https://x.com"},
		{"agentic_fetch", "agentic_fetch", `{"url":"https://x.com"}`, "https://x.com"},
		{"download with file_path", "download", `{"url":"https://x.com","file_path":"/tmp/y"}`, "/tmp/y"},
		{"download without file_path", "download", `{"url":"https://x.com"}`, "https://x.com"},
		{"bash → command", "bash", `{"command":"echo"}`, "echo"},
		{"bash multiline → first line", "bash", "{\"command\":\"cd x\\nmake\"}", "cd x"},
		{"sourcegraph → query", "sourcegraph", `{"query":"x"}`, "x"},
		{"agent → description", "agent", `{"description":"refactor","prompt":"p"}`, "refactor"},
		{"task → prompt fallback", "task", `{"prompt":"audit"}`, "audit"},
		{"empty input → no hint", "view", "", ""},
		{"unparseable → no hint", "view", "{not-json", ""},
		{"case insensitive", "VIEW", `{"file_path":"a.go"}`, "a.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := toolResultOriginHint(c.tool, c.input)
			assert.Equal(t, c.want, got, "tool=%s", c.tool)
		})
	}
}

func TestBuildToolCallContext(t *testing.T) {
	// The context map must (a) index every ToolCall in the slice by ID,
	// (b) preserve later overwrites if the same ID appears twice
	// (defensive — the agent should never emit duplicates, but the index
	// must not panic or pick arbitrarily), and (c) skip ToolCall parts
	// with empty IDs (we have nothing to key on).
	tc1 := someToolCall("id-1", "bash", `{"command":"ls"}`)
	tc2 := someToolCall("id-2", "view", `{"file_path":"a.go"}`)
	tcEmpty := someToolCall("", "edit", `{"file_path":"b.go"}`)
	tcDup := someToolCall("id-1", "bash", `{"command":"pwd"}`)

	msgs := []message.Message{
		{ID: "m1", Role: message.Assistant, Parts: []message.ContentPart{tc1, tc2}},
		{ID: "m2", Role: message.Assistant, Parts: []message.ContentPart{tcEmpty, tcDup}},
		{ID: "m3", Role: message.Tool, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "id-1", Content: "x"},
		}},
	}
	ctx := buildToolCallContext(msgs)

	require.Len(t, ctx, 2, "empty-ID ToolCall must be skipped, no Tool-role indexing")
	assert.Equal(t, "bash", ctx["id-1"].name)
	assert.Equal(t, `{"command":"pwd"}`, ctx["id-1"].input, "later duplicate overwrites earlier")
	assert.Equal(t, "view", ctx["id-2"].name)
	assert.Equal(t, `{"file_path":"a.go"}`, ctx["id-2"].input)
	_, exists := ctx[""]
	assert.False(t, exists, "empty-ID entry must not be in the map")
}

func TestLookupToolCallOrigin(t *testing.T) {
	ctx := map[string]toolCallOrigin{
		"id-1": {name: "bash", input: `{"command":"ls"}`},
	}
	name, input := lookupToolCallOrigin(ctx, "id-1")
	assert.Equal(t, "bash", name)
	assert.Equal(t, `{"command":"ls"}`, input)

	name, input = lookupToolCallOrigin(ctx, "missing")
	assert.Equal(t, "", name)
	assert.Equal(t, "", input)

	// Nil map is a valid input (legacy callsites that don't build a context).
	name, input = lookupToolCallOrigin(nil, "id-1")
	assert.Equal(t, "", name)
	assert.Equal(t, "", input)
}

// someToolCall is a tiny test helper so the buildToolCallContext test
// reads cleanly without literal struct noise.
func someToolCall(id, name, input string) message.ToolCall {
	return message.ToolCall{ID: id, Name: name, Input: input}
}

func TestTruncatePreview(t *testing.T) {
	assert.Equal(t, "abc", truncatePreview("abc", 10))
	// At-max stays as-is; over-max gets ellipsised.
	assert.Equal(t, "abcdefghij", truncatePreview("abcdefghij", 10))
	assert.Equal(t, "abcdefghi…", truncatePreview("abcdefghijk", 10))
	assert.Equal(t, "", truncatePreview("abc", 0))
	assert.Equal(t, "a", truncatePreview("abc", 1))
	// Multibyte safety: each rune counts as 1.
	assert.Equal(t, "абв", truncatePreview("абв", 5))
	assert.Equal(t, "абвг…", truncatePreview("абвгдеёж", 5))
}

func TestStringField(t *testing.T) {
	m := map[string]any{
		"a": "hello",
		"b": "  trimmed  ",
		"c": 42,
		"d": "",
	}
	assert.Equal(t, "hello", stringField(m, "a"))
	assert.Equal(t, "trimmed", stringField(m, "b"))
	assert.Equal(t, "", stringField(m, "c"))
	assert.Equal(t, "", stringField(m, "d"))
	assert.Equal(t, "", stringField(m, "missing"))
}

func TestFirstLine(t *testing.T) {
	assert.Equal(t, "", firstLine(""))
	assert.Equal(t, "", firstLine("   \n\t  "))
	assert.Equal(t, "hello", firstLine("hello"))
	assert.Equal(t, "hello", firstLine("  hello  "))
	assert.Equal(t, "first", firstLine("first\nsecond\nthird"))
	// Skips leading blank lines.
	assert.Equal(t, "real", firstLine("\n\n  \n  real\nmore"))
	// Carriage returns get trimmed off as whitespace.
	assert.Equal(t, "abc", firstLine("abc\r\ndef"))
}

func TestIntField(t *testing.T) {
	m := map[string]any{
		"a": float64(42),
		"b": 100,
		"c": int64(7),
		"d": "not a number",
	}
	assert.Equal(t, 42, intField(m, "a"))
	assert.Equal(t, 100, intField(m, "b"))
	assert.Equal(t, 7, intField(m, "c"))
	assert.Equal(t, 0, intField(m, "d"))
	assert.Equal(t, 0, intField(m, "missing"))
}
