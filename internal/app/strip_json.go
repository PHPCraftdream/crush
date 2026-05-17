package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Fork-only file (orchestrator UX): the JSON-envelope stripper used by
// `crush run --json` / `--format json` to defang a persistent model
// failure mode where the assistant wraps its supposed-to-be-raw-JSON
// final_text in a markdown ```json fence and/or a prose preamble.
// Inert to upstream — see CHANGELOG.fork.md (Section 4.J).

// ErrInvalidStripJSON is returned by stripAndValidateJSON when the
// extracted candidate (or the original text, if nothing could be
// stripped) is not parseable JSON. Callers should surface it via the
// runResult.Error/ExitReason so the orchestrator stops trusting the
// envelope's final_text. Wrapped with the underlying json error.
var ErrInvalidStripJSON = errors.New("model output is not valid JSON")

// stripAndValidateJSON is the entry point the runtime should prefer
// over stripJSONEnvelope when the operator explicitly asked for JSON
// (i.e. --json or --format json was passed). It runs the strip, then
// validates the result with json.Valid. Returns:
//
//   - cleaned: the stripped JSON when validation passed; the ORIGINAL
//     untouched text when validation failed (so the orchestrator can
//     still see the model's raw reply for debugging).
//   - notes:   the prose preamble/suffix removed by strip on success;
//     the stripped (but invalid) candidate on failure so the operator
//     can compare original vs strip output side by side.
//   - err:     nil on success; wraps ErrInvalidStripJSON with a
//     position-bearing json.SyntaxError on failure. The caller turns
//     this into runResult.Error + ExitReason="invalid_json" so a
//     downstream `jq .final_text` script reliably fails fast instead
//     of consuming structurally broken output.
//
// Rationale: the previous behaviour silently returned the stripped
// candidate when it was internally balanced (matching `{...}` walker)
// but not actually valid JSON — a real shamir-db audit run hit this
// when the model forgot a `]` before `,"post_flight":`, the envelope
// reported success, and an hour was spent in node-repl chasing it.
// See the user feedback dated 2026-05-17.
func stripAndValidateJSON(text string) (cleaned, notes string, err error) {
	stripped, notes := stripJSONEnvelope(text)
	if json.Valid([]byte(stripped)) {
		return stripped, notes, nil
	}
	// Validation failed. Surface the original to the orchestrator
	// (so manual inspection / re-prompting can see exactly what the
	// model produced) and put our best-effort strip into notes so the
	// operator can see what the parser tried.
	syntaxErr := jsonSyntaxErrorOf(stripped)
	return text, stripped, fmt.Errorf("%w: %s", ErrInvalidStripJSON, syntaxErr)
}

// jsonSyntaxErrorOf returns a human-readable description of what makes
// `s` invalid JSON. Uses json.Unmarshal because json.Valid only returns
// a bool. The returned string includes the byte offset so the operator
// can jump to it in the original output (assistant_notes carries it).
func jsonSyntaxErrorOf(s string) string {
	var v any
	err := json.Unmarshal([]byte(s), &v)
	if err == nil {
		return "unexpected: json.Valid said invalid but Unmarshal succeeded"
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("%s (at byte offset %d)", se.Error(), se.Offset)
	}
	return err.Error()
}

// stripJSONEnvelope returns (cleaned, notes).
//
// cleaned is the best-effort raw JSON value extracted from text — the
// substring starting at the first '{' or '[' and ending at the matching
// closing brace/bracket (matched naively but quote-aware).
//
// notes is everything we removed: the prose preamble before the JSON
// and the trailing prose/fence after it. Empty when text was already
// clean (no fence, no preamble, no trailing content).
//
// If we cannot find a balanced JSON value, we return (text, "") so the
// caller still has *something* and the orchestrator can see the raw
// reply via final_text. We never panic and we never lose data: the
// pre-strip text is always reconstructable as notes-prefix + cleaned +
// notes-suffix when notes != "".
func stripJSONEnvelope(text string) (cleaned, notes string) {
	original := text
	// Track fence boundaries separately so when the model wrapped the
	// JSON in ```json … ``` with nothing else, the fence markers
	// themselves do not get surfaced as "notes".
	var fenceOpenEnd, fenceCloseStart = -1, -1
	if openEnd, closeStart, ok := findJSONFenceBounds(text); ok {
		fenceOpenEnd = openEnd
		fenceCloseStart = closeStart
		text = text[openEnd:closeStart]
	}
	start, end, ok := findBalancedJSONValue(text)
	if !ok {
		return original, ""
	}
	cleaned = text[start:end]
	if cleaned == original {
		return original, ""
	}
	// Build prefix/suffix from the ORIGINAL, but if we used a fence,
	// trim everything that belongs to the fence machinery (the opening
	// "```json\n" line and the closing "```" line) so genuinely
	// content-free wrappers yield empty notes.
	var rawPrefix, rawSuffix string
	if fenceOpenEnd >= 0 {
		rawPrefix = original[:fenceOpenEnd-len("```json\n")+0] // safe slice; cleaned via TrimSpace below
		// Recompute prefix conservatively: everything before the opening fence line.
		fenceLineStart := strings.LastIndex(original[:fenceOpenEnd], "```json")
		if fenceLineStart < 0 {
			fenceLineStart = strings.LastIndex(strings.ToLower(original[:fenceOpenEnd]), "```json")
		}
		if fenceLineStart >= 0 {
			rawPrefix = original[:fenceLineStart]
		}
		// Suffix: everything after the closing ``` line.
		closeLineEnd := fenceCloseStart + len("```")
		if closeLineEnd <= len(original) {
			rawSuffix = original[closeLineEnd:]
		}
	} else {
		idx := strings.Index(original, cleaned)
		if idx < 0 {
			// Shouldn't happen — defend with whole-text removal.
			notes = strings.TrimSpace(strings.Replace(original, cleaned, "", 1))
			return cleaned, notes
		}
		rawPrefix = original[:idx]
		rawSuffix = original[idx+len(cleaned):]
	}
	prefix := strings.TrimSpace(rawPrefix)
	suffix := strings.TrimSpace(rawSuffix)
	switch {
	case prefix != "" && suffix != "":
		notes = prefix + "\n\n[…JSON…]\n\n" + suffix
	case prefix != "":
		notes = prefix
	case suffix != "":
		notes = suffix
	}
	return cleaned, notes
}

// findJSONFenceBounds locates the first ```json (or ```JSON) markdown
// code fence in text. Returns (contentStart, contentEnd, true) where
// contentStart is the byte after the opening fence line's newline and
// contentEnd is the byte index of the closing "```". The opening
// fence line itself is from text[lastIndex("```json") : contentStart]
// and the closing fence line is from contentEnd : contentEnd+3.
//
// Returns ok=false when no recognisable fenced JSON block is present.
func findJSONFenceBounds(text string) (contentStart, contentEnd int, ok bool) {
	lower := strings.ToLower(text)
	openIdx := strings.Index(lower, "```json")
	if openIdx < 0 {
		return 0, 0, false
	}
	nl := strings.IndexByte(text[openIdx:], '\n')
	if nl < 0 {
		return 0, 0, false
	}
	contentStart = openIdx + nl + 1
	closeRel := strings.Index(text[contentStart:], "```")
	if closeRel < 0 {
		return 0, 0, false
	}
	contentEnd = contentStart + closeRel
	return contentStart, contentEnd, true
}

// findBalancedJSONValue walks text from the first '{' or '[' and finds
// the matching closing brace/bracket, honouring string literals
// (including backslash-escaped quotes). Returns ok=false if no
// balanced value can be matched.
//
// This is intentionally a tolerant walk, not a JSON parser: a model
// that emits subtly malformed JSON (trailing comma, single quotes)
// will still get its content surfaced; downstream `jq` / parser will
// then complain in the orchestrator's own pipeline, which is the
// right place.
func findBalancedJSONValue(text string) (start, end int, ok bool) {
	// Locate the first JSON-opener that is not inside a backtick
	// (cheap heuristic — the fence-strip already removed the markdown
	// case, so what remains is plain prose + JSON).
	start = -1
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '{' || c == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	open := text[start]
	var close byte = '}'
	if open == '[' {
		close = ']'
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return start, i + 1, true
			}
		}
	}
	return 0, 0, false
}
