package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolPreviewMaxLen caps the displayed length of a tool-call argument
// preview so a single long bash command does not dominate the screen.
const toolPreviewMaxLen = 80

// toolResultPreviewMaxLen caps the first-line preview of a tool result.
const toolResultPreviewMaxLen = 200

// formatToolCallPreview extracts a short, human-friendly summary of the
// most informative parameter from a tool call's JSON input. The result is
// rendered next to "[tool: <name>]" in sessions last/tail/pick output so
// the operator sees WHAT the agent is doing, not just THAT it called a
// tool. Returns "" when the input is empty or unparseable.
//
// Per-tool field priority (best-known field first; fallback to first
// non-empty string field in the input map):
//
//	bash                                → command
//	view                                → file_path[:offset+limit]
//	edit / multiedit / write            → file_path
//	grep                                → pattern
//	glob                                → pattern
//	ls                                  → path
//	fetch / web_fetch / agentic_fetch   → url
//	download                            → url → file_path
//	sourcegraph                         → query
//	agent / sub_agent / task            → description or prompt
//	todo / todowrite                    → "<N> todos"
func formatToolCallPreview(name, input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		// Not JSON or partial stream — show the raw input truncated.
		return truncatePreview(strings.TrimSpace(input), toolPreviewMaxLen)
	}

	switch strings.ToLower(name) {
	case "bash":
		if v := stringField(params, "command"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
	case "view":
		if path := stringField(params, "file_path"); path != "" {
			offset := intField(params, "offset")
			limit := intField(params, "limit")
			switch {
			case offset > 0 && limit > 0:
				return truncatePreview(fmt.Sprintf("%s [%d:+%d]", path, offset, limit), toolPreviewMaxLen)
			case limit > 0:
				return truncatePreview(fmt.Sprintf("%s [:%d]", path, limit), toolPreviewMaxLen)
			default:
				return truncatePreview(path, toolPreviewMaxLen)
			}
		}
	case "edit", "multiedit", "write":
		if v := stringField(params, "file_path"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
	case "grep":
		pat := stringField(params, "pattern")
		path := stringField(params, "path")
		if pat != "" && path != "" {
			return truncatePreview(fmt.Sprintf("%q in %s", pat, path), toolPreviewMaxLen)
		}
		if pat != "" {
			return truncatePreview(fmt.Sprintf("%q", pat), toolPreviewMaxLen)
		}
	case "glob":
		if v := stringField(params, "pattern"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
	case "ls":
		if v := stringField(params, "path"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
	case "fetch", "web_fetch", "agentic_fetch":
		if v := stringField(params, "url"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
	case "download":
		url := stringField(params, "url")
		dst := stringField(params, "file_path")
		switch {
		case url != "" && dst != "":
			return truncatePreview(fmt.Sprintf("%s → %s", url, dst), toolPreviewMaxLen)
		case url != "":
			return truncatePreview(url, toolPreviewMaxLen)
		case dst != "":
			return truncatePreview(dst, toolPreviewMaxLen)
		}
	case "sourcegraph":
		if v := stringField(params, "query"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
	case "agent", "sub_agent", "task":
		if v := stringField(params, "description"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
		if v := stringField(params, "prompt"); v != "" {
			return truncatePreview(v, toolPreviewMaxLen)
		}
	case "todo", "todowrite":
		// Hand-crafted: count items rather than dump JSON.
		if todos, ok := params["todos"].([]any); ok {
			return fmt.Sprintf("%d todos", len(todos))
		}
	}

	// Generic fallback: the first non-empty string field.
	for _, key := range orderedKeys(params) {
		if s, ok := params[key].(string); ok && strings.TrimSpace(s) != "" {
			return truncatePreview(fmt.Sprintf("%s=%s", key, s), toolPreviewMaxLen)
		}
	}
	return ""
}

// formatToolResultPreview returns a one-line summary of a tool result's
// content. If the content fits in toolResultPreviewMaxLen the whole
// (single-line-collapsed) string is returned; otherwise the first line is
// returned with a "(N more lines)" suffix when the content is multiline.
// Returns "" for empty or whitespace-only content.
func formatToolResultPreview(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	first := strings.TrimSpace(lines[0])
	if len(lines) == 1 {
		return truncatePreview(first, toolResultPreviewMaxLen)
	}
	suffix := fmt.Sprintf(" (+%d lines)", len(lines)-1)
	available := toolResultPreviewMaxLen - len(suffix)
	if available < 20 {
		available = 20
	}
	return truncatePreview(first, available) + suffix
}

// truncatePreview shortens s to max runes, appending an ellipsis when
// truncation actually happened. Counts runes (not bytes) so multibyte
// characters don't get cut mid-encoding.
func truncatePreview(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

// stringField looks up a key in the parsed params map and returns its
// string value, or "" if missing / not a string / empty after trim.
func stringField(params map[string]any, key string) string {
	v, ok := params[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// intField returns the int value at key, supporting both float64 (JSON's
// native number) and int. Returns 0 when missing or wrong type.
func intField(params map[string]any, key string) int {
	v, ok := params[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// orderedKeys returns the map keys in a stable order so the fallback
// preview is deterministic across calls — important for tests and for not
// confusing the operator with shifting field names between renders.
func orderedKeys(params map[string]any) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	// Stable alphabetical order.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
