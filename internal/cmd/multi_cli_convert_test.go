package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// parseSlashCommandSource
// ---------------------------------------------------------------------------

func TestParseSlashCommandSource_Basic(t *testing.T) {
	src := "---\n" +
		"description: Do the thing\n" +
		"---\n" +
		"\n" +
		"Body line 1\n" +
		"Body line 2 with $ARGUMENTS\n"

	desc, body, err := parseSlashCommandSource(src)
	require.NoError(t, err)
	assert.Equal(t, "Do the thing", desc)
	assert.Equal(t, "Body line 1\nBody line 2 with $ARGUMENTS\n", body)
}

func TestParseSlashCommandSource_DescriptionWithColon(t *testing.T) {
	src := "---\n" +
		"description: Do the thing: carefully, and with colons: like this\n" +
		"---\n" +
		"\n" +
		"Body\n"

	desc, body, err := parseSlashCommandSource(src)
	require.NoError(t, err)
	assert.Equal(t, "Do the thing: carefully, and with colons: like this", desc)
	assert.Equal(t, "Body\n", body)
}

func TestParseSlashCommandSource_TrimsLeadingBlankLines(t *testing.T) {
	src := "---\n" +
		"description: X\n" +
		"---\n" +
		"\n\n\n" +
		"Body starts here\n"

	_, body, err := parseSlashCommandSource(src)
	require.NoError(t, err)
	assert.Equal(t, "Body starts here\n", body)
}

func TestParseSlashCommandSource_RealTemplates(t *testing.T) {
	desc1, body1, err := parseSlashCommandSource(claudeSlashCommandTemplate)
	require.NoError(t, err)
	assert.NotEmpty(t, desc1)
	assert.Contains(t, body1, "$ARGUMENTS")

	desc2, body2, err := parseSlashCommandSource(claudeFallbackCommandTemplate)
	require.NoError(t, err)
	assert.NotEmpty(t, desc2)
	assert.Contains(t, body2, "$ARGUMENTS")
}

func TestParseSlashCommandSource_MissingOpeningDelimiter(t *testing.T) {
	_, _, err := parseSlashCommandSource("no front matter here\n")
	assert.Error(t, err)
}

func TestParseSlashCommandSource_MissingDescriptionLine(t *testing.T) {
	src := "---\n" +
		"notdescription: X\n" +
		"---\n" +
		"Body\n"
	_, _, err := parseSlashCommandSource(src)
	assert.Error(t, err)
}

func TestParseSlashCommandSource_MissingClosingDelimiter(t *testing.T) {
	src := "---\n" +
		"description: X\n" +
		"Body without closing delimiter\n"
	_, _, err := parseSlashCommandSource(src)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// renderFrontMatterMD
// ---------------------------------------------------------------------------

func TestRenderFrontMatterMD(t *testing.T) {
	got := renderFrontMatterMD("<!-- sentinel:v1 -->", "My desc", "Hello $ARGUMENTS world", "{{args}}")
	want := "<!-- sentinel:v1 -->\n---\ndescription: My desc\n---\n\nHello {{args}} world"
	assert.Equal(t, want, got)
}

// ---------------------------------------------------------------------------
// toGeminiTOML
// ---------------------------------------------------------------------------

func TestToGeminiTOML_Basic(t *testing.T) {
	got, err := toGeminiTOML(`Say "hi"`, "Hello $ARGUMENTS world")
	require.NoError(t, err)
	assert.Contains(t, got, "# crush-slash-command:v1")
	assert.Contains(t, got, `description = "Say \"hi\""`)
	assert.Contains(t, got, `prompt = """`)
	assert.Contains(t, got, "Hello {{args}} world")
	assert.NotContains(t, got, "$ARGUMENTS")
}

func TestToGeminiTOML_ErrorsOnTripleQuoteInBody(t *testing.T) {
	_, err := toGeminiTOML("desc", `body with """ triple quotes`)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// toSkillMD
// ---------------------------------------------------------------------------

func TestToSkillMD(t *testing.T) {
	got := toSkillMD("crush", "My description", "Body text with $ARGUMENTS placeholder")
	assert.True(t, strings.HasPrefix(got, claudeSlashCommandSentinel+"\n"))
	assert.Contains(t, got, "name: crush\n")
	assert.Contains(t, got, "description: My description\n")
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "Body text with $ARGUMENTS placeholder")
	// The $ARGUMENTS placeholder in the body itself must NOT be rewritten.
	assert.Contains(t, got, "treat it exactly as `$ARGUMENTS` below would have been substituted")
}

// ---------------------------------------------------------------------------
// Regression guard: source templates must never contain literal triple
// quotes, since toGeminiTOML embeds bodies into a TOML triple-quoted
// string and would otherwise silently produce broken TOML.
// ---------------------------------------------------------------------------

func TestNoTripleQuotesInSource(t *testing.T) {
	assert.NotContains(t, claudeSlashCommandTemplate, `"""`)
	assert.NotContains(t, claudeFallbackCommandTemplate, `"""`)
}
