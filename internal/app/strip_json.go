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

// Fork patch (multi-JSON extractor): findAllBalancedJSONValues scans text
// for ALL balanced JSON values (objects or arrays), advancing past each
// match. Returns them in source order. The existing findBalancedJSONValue
// walker is reused per iteration — quote-awareness is already handled.
func findAllBalancedJSONValues(text string) []string {
	var results []string
	offset := 0
	for {
		remaining := text[offset:]
		start, end, ok := findBalancedJSONValue(remaining)
		if !ok {
			break
		}
		results = append(results, remaining[start:end])
		offset += end
	}
	return results
}

// Fork patch (multi-JSON extractor): stripAndExtractJSON is the new
// runtime entry point that replaces stripAndValidateJSON. It handles
// the common small-model failure mode where the model emits prose
// preamble + one JSON value, or even MULTIPLE JSON values separated
// by prose (observed with GLM-5-turbo and friends).
//
// Behaviour:
//   - 0 valid JSON values found → ErrInvalidStripJSON, original text
//     returned in cleaned (same as before).
//   - 1 valid JSON value found  → returned as cleaned, notes carries
//     the prose preamble/suffix that was stripped.
//   - ≥2 valid JSON values found → returned as a JSON array wrapper
//     `[json1, json2, ...]` (preserving each value as-is). notes
//     includes a one-line marker "extracted N JSON values, M chars of
//     prose discarded" for operator forensics.
func stripAndExtractJSON(text string) (cleaned, notes string, err error) {
	// Fork patch: run fence-strip first (same as before).
	inner := text
	fenceOpenEnd, fenceCloseStart := -1, -1
	if openEnd, closeStart, ok := findJSONFenceBounds(text); ok {
		fenceOpenEnd = openEnd
		fenceCloseStart = closeStart
		inner = text[openEnd:closeStart]
	}

	// Fork patch: find ALL balanced JSON values in the (possibly
	// fence-stripped) text.
	candidates := findAllBalancedJSONValues(inner)

	// Fork patch: validate each candidate with json.Valid.
	var valid []string
	for _, c := range candidates {
		if json.Valid([]byte(c)) {
			valid = append(valid, c)
		}
	}

	switch len(valid) {
	case 0:
		// No valid JSON found — same behaviour as before.
		syntaxErr := jsonSyntaxErrorOf(text)
		return text, "", fmt.Errorf("%w: %s", ErrInvalidStripJSON, syntaxErr)
	case 1:
		// Exactly one valid JSON — return it with notes about stripped prose.
		cleaned = valid[0]
		proseChars := len(text) - len(cleaned)
		notes = buildStripNotes(text, fenceOpenEnd, fenceCloseStart, cleaned, proseChars)
		return cleaned, notes, nil
	default:
		// Fork patch: multiple valid JSON values — wrap as array.
		// Build the array by joining the values with commas.
		var buf strings.Builder
		buf.WriteByte('[')
		for i, v := range valid {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(v)
		}
		buf.WriteByte(']')
		cleaned = buf.String()

		// The wrap MUST be valid JSON — verify.
		if !json.Valid([]byte(cleaned)) {
			// Should never happen since each element is valid JSON,
			// but fall back to single-value mode if it does.
			syntaxErr := jsonSyntaxErrorOf(cleaned)
			return text, "", fmt.Errorf("%w: multi-value wrap failed validation: %s", ErrInvalidStripJSON, syntaxErr)
		}

		proseChars := len(text) - len(cleaned)
		if proseChars < 0 {
			proseChars = 0
		}
		notes = fmt.Sprintf("extracted %d JSON values, %d chars of prose discarded", len(valid), proseChars)
		if fenceOpenEnd >= 0 || proseChars > 0 {
			notes += " (" + buildStripNotes(text, fenceOpenEnd, fenceCloseStart, cleaned, proseChars) + ")"
		}
		return cleaned, notes, nil
	}
}

// Fork patch (multi-JSON extractor): buildStripNotes reconstructs the
// prose preamble/suffix notes for the single-value case, reusing the
// same logic as stripJSONEnvelope but without the multi-value wrapping.
func buildStripNotes(original string, fenceOpenEnd, fenceCloseStart int, cleaned string, proseChars int) string {
	if proseChars == 0 {
		return ""
	}
	var rawPrefix, rawSuffix string
	if fenceOpenEnd >= 0 {
		fenceLineStart := strings.LastIndex(original[:fenceOpenEnd], "```json")
		if fenceLineStart < 0 {
			fenceLineStart = strings.LastIndex(strings.ToLower(original[:fenceOpenEnd]), "```json")
		}
		if fenceLineStart >= 0 {
			rawPrefix = original[:fenceLineStart]
		}
		closeLineEnd := fenceCloseStart + len("```")
		if closeLineEnd <= len(original) {
			rawSuffix = original[closeLineEnd:]
		}
	} else {
		idx := strings.Index(original, cleaned)
		if idx < 0 {
			return fmt.Sprintf("%d chars stripped", proseChars)
		}
		rawPrefix = original[:idx]
		rawSuffix = original[idx+len(cleaned):]
	}
	prefix := strings.TrimSpace(rawPrefix)
	suffix := strings.TrimSpace(rawSuffix)
	switch {
	case prefix != "" && suffix != "":
		return prefix + "\n\n[…JSON…]\n\n" + suffix
	case prefix != "":
		return prefix
	case suffix != "":
		return suffix
	default:
		return fmt.Sprintf("%d chars stripped", proseChars)
	}
}

// stripAndValidateJSON is kept for backward compatibility but is no
// longer the primary runtime entry point — stripAndExtractJSON is.
// Fork patch: retained because it is referenced in comments and tests.
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
	fenceOpenEnd, fenceCloseStart := -1, -1
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
