package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
			got := formatToolResultPreview(c.content)
			assert.Equal(t, c.want, got, "content=%q", c.content)
		})
	}
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
