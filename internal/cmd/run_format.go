package cmd

import (
	"fmt"
	"os"
	"strings"
)

// Fork-only file (concurrency / orchestrator UX): the --format and
// --agents flags exposed on `crush run` are inert to upstream and live
// here so a merge from origin/main never touches run.go's flag block.
// See CHANGELOG.fork.md (Section 4.J).

// formatPresetJSON is the canonical instruction appended to the user
// prompt when --format json is passed. The post-processor in app.go
// (stripAndValidateJSON) is the safety net when the model ignores it
// anyway. Wording tightened 2026-05-17 after the audit feedback
// showed glm-5.1 reliably writing a prose preamble despite the
// previous hint — the new version uses imperative voice, explains
// the machine-parsing consequence, and ends with a hard last-line
// instruction (most models attend to the LAST instruction more
// strongly than the first).
const formatPresetJSON = `## Output Format (mandatory, machine-parsed)

Your reply is parsed by ` + "`jq`" + ` on the final_text field. Any character
before the first ` + "`{`" + ` or ` + "`[`" + `, and any character after the matching
closing ` + "`}`" + ` or ` + "`]`" + `, causes a parse failure that aborts the wrapper
script. There is no human reading this turn's reply directly.

Rules:

1. Emit exactly one JSON value (object or array). Nothing else.
2. The very first character of your reply MUST be ` + "`{`" + ` or ` + "`[`" + `.
3. The very last character of your reply MUST be the matching ` + "`}`" + ` or ` + "`]`" + `.
4. Validate brackets balance and every ` + "`,`" + ` is followed by a key or value
   (no trailing commas, no missing ` + "`]`" + ` before another key).
5. No markdown code fence. No ` + "```json`" + `, no ` + "`````" + ` anywhere.
6. No prose preamble ("Here is...", "Let me compose...", "Now I'll output...").
7. No suffix ("Let me know if...", "Hope this helps.").
8. No explanations, sign-offs, or emojis outside of JSON string values.

Last line, repeated for attention: your reply starts with ` + "`{`" + ` or ` + "`[`" + ` and
ends with the matching ` + "`}`" + ` or ` + "`]`" + `. The wrapper does NOT strip prose for
you — invalid JSON fails the run.`

// resolveFormatHint turns the raw --format flag value into the text that
// will be appended to the user prompt. Returns ("", nil) when the flag
// was not passed.
//
// Supported forms:
//   - ""              → no hint (flag absent)
//   - "json"          → expand to formatPresetJSON
//   - "json-schema:<path>"
//     → read <path> as a JSON schema, build the prompt around it
//   - "@<path>"       → read <path> verbatim and use as the hint body
//     (lets the user keep multi-paragraph format specs in a file)
//   - any other text  → use as a freeform "Output format: <text>" hint
func resolveFormatHint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	switch {
	case raw == "json":
		return formatPresetJSON, nil
	case strings.HasPrefix(raw, "json-schema:"):
		path := strings.TrimPrefix(raw, "json-schema:")
		bts, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("--format json-schema: read %s: %w", path, err)
		}
		schema := strings.TrimSpace(string(bts))
		return formatPresetJSON + "\n\nThe JSON must conform to this schema:\n\n```json\n" + schema + "\n```", nil
	case strings.HasPrefix(raw, "@"):
		path := strings.TrimPrefix(raw, "@")
		bts, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("--format @file: read %s: %w", path, err)
		}
		return "## Output Format\n\n" + strings.TrimSpace(string(bts)), nil
	default:
		return "## Output Format\n\n" + raw, nil
	}
}

// agentsModePromptHint returns the user-prompt fragment associated with
// --agents=with-agents (nudging the model to fan out via the `agent`
// tool). The "single" mode is implemented by config mutation, not by a
// prompt, and "agent-allow" is the upstream default with no hint.
const agentsModePromptHint = `## Sub-agents

You have access to the ` + "`agent`" + ` tool. Use it to parallelise independent searches/lookups across multiple files or topics. The orchestrator explicitly requested fan-out for this run — prefer one ` + "`agent`" + ` dispatch per independent sub-task over doing them sequentially in your own turn.`

// composeUserPrompt joins the user's prompt with optional format and
// agents hints. The hints are appended (not prepended) so the user's
// original request stays at the top of the model's context, which most
// providers' attention curves still favour. Empty hints contribute
// nothing — no separator, no whitespace.
func composeUserPrompt(prompt, formatHint, agentsHint string) string {
	parts := []string{strings.TrimRight(prompt, "\n")}
	if formatHint != "" {
		parts = append(parts, formatHint)
	}
	if agentsHint != "" {
		parts = append(parts, agentsHint)
	}
	return strings.Join(parts, "\n\n")
}
