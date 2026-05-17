package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork-only tests (orchestrator UX): cover the pure-function pieces of
// the --format / --agents / --aggregation plumbing. Goal: regressions
// like the "invalid_json fires on --json alone" bug from session #3
// (2026-05-17) should be caught here before they reach prod.

// --- resolveFormatHint --------------------------------------------------

func TestResolveFormatHint_Empty(t *testing.T) {
	got, err := resolveFormatHint("")
	require.NoError(t, err)
	assert.Empty(t, got, "no flag => no hint => no prompt-bloat")
}

func TestResolveFormatHint_WhitespaceCountsAsEmpty(t *testing.T) {
	got, err := resolveFormatHint("   \n\t ")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestResolveFormatHint_JSONPreset(t *testing.T) {
	got, err := resolveFormatHint("json")
	require.NoError(t, err)
	assert.Equal(t, formatPresetJSON, got,
		"--format json must expand to the canonical preset verbatim")
}

func TestResolveFormatHint_JSONSchemaReadsFile(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "audit.json")
	require.NoError(t, os.WriteFile(schemaPath,
		[]byte(`{"type":"object","properties":{"findings":{"type":"array"}}}`), 0o644))

	got, err := resolveFormatHint("json-schema:" + schemaPath)
	require.NoError(t, err)
	assert.Contains(t, got, formatPresetJSON,
		"schema preset must still carry the base JSON-format rules")
	assert.Contains(t, got, `"findings"`,
		"schema body must be inlined into the prompt so the model sees it")
}

func TestResolveFormatHint_JSONSchemaMissingFile(t *testing.T) {
	_, err := resolveFormatHint("json-schema:/nonexistent/path/schema.json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "json-schema",
		"error must name the flag so the operator knows where to look")
}

func TestResolveFormatHint_AtFileVerbatim(t *testing.T) {
	dir := t.TempDir()
	hintPath := filepath.Join(dir, "shape.md")
	body := "Respond as YAML with keys: severity, file, fix."
	require.NoError(t, os.WriteFile(hintPath, []byte(body), 0o644))

	got, err := resolveFormatHint("@" + hintPath)
	require.NoError(t, err)
	assert.Contains(t, got, body, "@file hint body must be inlined verbatim")
	assert.Contains(t, got, "## Output Format", "must wrap in a heading")
}

func TestResolveFormatHint_AtFileMissing(t *testing.T) {
	_, err := resolveFormatHint("@/nonexistent/hint.md")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "@file")
}

func TestResolveFormatHint_FreeformText(t *testing.T) {
	got, err := resolveFormatHint("Output: bullets only")
	require.NoError(t, err)
	assert.Contains(t, got, "Output: bullets only",
		"freeform text passes through verbatim")
	assert.Contains(t, got, "## Output Format")
}

// --- composeUserPrompt --------------------------------------------------

func TestComposeUserPrompt_AllEmptyHints(t *testing.T) {
	got := composeUserPrompt("user prompt", "", "", "")
	assert.Equal(t, "user prompt", got,
		"no hints => no separator, no trailing newlines, no bloat")
}

func TestComposeUserPrompt_FormatHintOnly(t *testing.T) {
	got := composeUserPrompt("audit this", "## Output Format\n- raw JSON", "", "")
	assert.True(t, strings.HasPrefix(got, "audit this"),
		"user request stays at the top (attention favours start)")
	assert.Contains(t, got, "## Output Format")
}

func TestComposeUserPrompt_AllHintsAppendedInOrder(t *testing.T) {
	got := composeUserPrompt("user", "FORMAT-HINT", "AGENTS-HINT", "AGGREGATION-HINT")
	idxUser := strings.Index(got, "user")
	idxFormat := strings.Index(got, "FORMAT-HINT")
	idxAgents := strings.Index(got, "AGENTS-HINT")
	idxAggr := strings.Index(got, "AGGREGATION-HINT")
	require.GreaterOrEqual(t, idxUser, 0)
	require.GreaterOrEqual(t, idxFormat, 0)
	require.GreaterOrEqual(t, idxAgents, 0)
	require.GreaterOrEqual(t, idxAggr, 0)
	assert.Less(t, idxUser, idxFormat, "user prompt first")
	assert.Less(t, idxFormat, idxAgents, "format -> agents")
	assert.Less(t, idxAgents, idxAggr, "agents -> aggregation")
}

func TestComposeUserPrompt_StripsTrailingNewline(t *testing.T) {
	got := composeUserPrompt("user\n\n", "HINT", "", "")
	assert.False(t, strings.Contains(got, "user\n\n\n\nHINT"),
		"trailing newlines on the user prompt must not stack against the separator")
}

// --- aggregation hint constants are non-empty and have the expected key terms

func TestAggregationConcatPromptHint_MentionsVerbatim(t *testing.T) {
	assert.Contains(t, aggregationConcatPromptHint, "verbatim",
		"concat hint must explicitly request verbatim sub-agent text")
	assert.Contains(t, aggregationConcatPromptHint, "summarise",
		"hint must name the failure mode it defends against")
}

func TestAggregationAttachPromptHint_MentionsBriefWrapUp(t *testing.T) {
	assert.Contains(t, aggregationAttachPromptHint, "sub_agent_outputs",
		"attach hint must reference the envelope field by name so the model knows where its output goes")
	assert.Contains(t, aggregationAttachPromptHint, "wrap-up",
		"hint must instruct a brief wrap-up only")
}

func TestAgentsModePromptHint_MentionsAgentTool(t *testing.T) {
	assert.Contains(t, agentsModePromptHint, "agent",
		"with-agents hint must reference the agent tool by name")
	assert.Contains(t, agentsModePromptHint, "parallelise",
		"must explain WHY (parallelisation)")
}

// --- formatPresetJSON ---------------------------------------------------

func TestFormatPresetJSON_HasHardLastLine(t *testing.T) {
	// "Models attend to the LAST instruction more strongly than the
	// first" — session #3 audit feedback. The preset MUST close with
	// a hard rule (either repeating the start-with-{-or-[ shape OR
	// the validation-failure consequence). Anything weaker leaves a
	// hole for the model to write a trailing sign-off.
	lines := strings.Split(strings.TrimSpace(formatPresetJSON), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	last := strings.ToLower(lines[len(lines)-1])
	hasShape := strings.Contains(last, "{") || strings.Contains(last, "[")
	hasValidation := strings.Contains(last, "invalid") || strings.Contains(last, "fails")
	assert.True(t, hasShape || hasValidation,
		"last line must be a hard rule (shape or validation-failure); got %q", lines[len(lines)-1])
}

func TestFormatPresetJSON_ForbidsFences(t *testing.T) {
	assert.Contains(t, formatPresetJSON, "No markdown code fence",
		"preset must explicitly forbid markdown fences — main failure mode")
}
