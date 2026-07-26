package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderEffortsOverview_NoArg covers the no-argument output: it must
// state both syntaxes (short codes + raw @effort), call out that @effort is
// unvalidated, state the Claude-only short-code asymmetry, and mention every
// provider's collapsing behavior — especially Z.AI's low/medium/high → high.
func TestRenderEffortsOverview_NoArg(t *testing.T) {
	out := renderEffortsOverview()

	// Two syntaxes.
	assert.Contains(t, out, "Short codes")
	assert.Contains(t, out, "@effort")
	assert.Contains(t, out, "UNVALIDATED")

	// Claude-only short-code asymmetry.
	assert.Contains(t, out, "ASYMMETRY")
	assert.Contains(t, out, "local-cli/")
	assert.Contains(t, out, "`glm5_2xx`")
	assert.Contains(t, out, "has no short code for effort")

	// Z.AI collapsing — the single most surprising fact.
	assert.Contains(t, out, "Z.AI")
	assert.Contains(t, out, `unset, low, medium, high     -> reasoning_effort: "high"`)
	assert.Contains(t, out, "indistinguishable from high")

	// DeepSeek's differing unset default should also be documented.
	assert.Contains(t, out, "DeepSeek")
	assert.Contains(t, out, "UNSET effort means thinking is OFF")

	// io.net / Alibaba / hyper coverage (boolean-only providers).
	assert.Contains(t, out, "io.net")
	assert.Contains(t, out, "Alibaba Singapore")
	assert.Contains(t, out, "hyper")
}

// TestRenderEffortsForModel_ZAI verifies a Z.AI model lookup by atom key
// states the low/medium→high collapsing explicitly, since that is the fact
// most likely to waste a user's time if it's missing.
func TestRenderEffortsForModel_ZAI(t *testing.T) {
	out, err := renderEffortsForModel("glm5_2")
	require.NoError(t, err)

	assert.Contains(t, out, "GLM 5.2")
	assert.Contains(t, out, "zai/glm-5.2")
	assert.Contains(t, out, "indistinguishable from high")
	assert.Contains(t, out, `unset, low, medium, high     -> reasoning_effort: "high"`)
	assert.Contains(t, out, "xhigh, max, ultracode        -> reasoning_effort: \"max\"")

	// Must show the raw @effort command form, not a short code (none exist).
	assert.Contains(t, out, "crush models use zai/glm-5.2@<level> <small>")
	assert.Contains(t, out, "crush models use zai/glm-5.2@off <small>")
	assert.Contains(t, out, "crush models use zai/glm-5.2@max <small>")
}

// TestRenderEffortsForModel_ZAI_RawProviderModel verifies the same lookup
// also works when addressed as raw "provider/model" instead of the atom key.
func TestRenderEffortsForModel_ZAI_RawProviderModel(t *testing.T) {
	out, err := renderEffortsForModel("zai/glm-5.2")
	require.NoError(t, err)
	assert.Contains(t, out, "atom: glm5_2")
	assert.Contains(t, out, "indistinguishable from high")
}

// TestRenderEffortsForModel_Claude verifies a Claude/local-cli model lookup
// (via a short code, matching how users actually invoke it) shows the
// atom-level command syntax and the CLI-validated framing, not the raw
// @effort form (Claude atoms use short codes, not @effort).
func TestRenderEffortsForModel_Claude(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high", "xhigh", "max"})()

	out, err := renderEffortsForModel("fl")
	require.NoError(t, err)

	assert.Contains(t, out, "atom: fable")
	assert.Contains(t, out, "local-cli/cli-claude-fable")
	assert.Contains(t, out, "claude` CLI")
	assert.Contains(t, out, "fable-low")
	assert.Contains(t, out, "fable-high")
	assert.Contains(t, out, "fable-max")
	assert.Contains(t, out, "crush models use fable-high <small>")

	// Must NOT suggest the @effort form for a Claude atom.
	assert.NotContains(t, out, "fable@")
}

// TestRenderEffortsForModel_Claude_ByAtomKey verifies the same lookup by
// bare atom key (no effort suffix) also resolves.
func TestRenderEffortsForModel_Claude_ByAtomKey(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high"})()

	out, err := renderEffortsForModel("fable")
	require.NoError(t, err)
	assert.Contains(t, out, "atom: fable")
	assert.Contains(t, out, "fable-low")
	assert.Contains(t, out, "fable-medium")
	assert.Contains(t, out, "fable-high")
}

// TestRenderEffortsForModel_UnknownModel verifies the error path for a
// model/atom/short-code that resolves to nothing.
func TestRenderEffortsForModel_UnknownModel(t *testing.T) {
	_, err := renderEffortsForModel("totally-fake-model-xyz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a recognized atom, short code, or provider/model")
	assert.Contains(t, err.Error(), "crush models list")
}

// TestResolveEffortTarget_Variants exercises the three accepted input
// shapes: atom key, full short code, and raw provider/model.
func TestResolveEffortTarget_Variants(t *testing.T) {
	t.Run("atom key", func(t *testing.T) {
		target, ok := resolveEffortTarget("glm5_1")
		require.True(t, ok)
		assert.Equal(t, "glm5_1", target.AtomKey)
		assert.Equal(t, "zai", target.Provider)
		assert.Equal(t, "glm-5.1", target.Model)
	})

	t.Run("short code", func(t *testing.T) {
		defer setMockEffortLevels([]string{"low", "medium", "high"})()
		target, ok := resolveEffortTarget("hl")
		require.True(t, ok)
		assert.Equal(t, "haiku", target.AtomKey)
		assert.Equal(t, "local-cli", target.Provider)
	})

	t.Run("raw provider/model not in registry", func(t *testing.T) {
		target, ok := resolveEffortTarget("openai/gpt-5")
		require.True(t, ok)
		assert.Empty(t, target.AtomKey)
		assert.Equal(t, "openai", target.Provider)
		assert.Equal(t, "gpt-5", target.Model)
	})

	t.Run("raw provider/model with @effort suffix ignored for lookup", func(t *testing.T) {
		target, ok := resolveEffortTarget("zai/glm-5.2@max")
		require.True(t, ok)
		assert.Equal(t, "glm5_2", target.AtomKey)
		assert.Equal(t, "zai", target.Provider)
		assert.Equal(t, "glm-5.2", target.Model)
	})

	t.Run("unrecognized", func(t *testing.T) {
		_, ok := resolveEffortTarget("nonsense-not-a-model")
		assert.False(t, ok)
	})
}

// TestModelsEffortsCmd_RegisteredAsSubcommand sanity-checks that init()
// registered the new command under `crush models`.
func TestModelsEffortsCmd_RegisteredAsSubcommand(t *testing.T) {
	found := false
	for _, sub := range modelsCmd.Commands() {
		if strings.HasPrefix(sub.Use, "efforts") {
			found = true
			break
		}
	}
	assert.True(t, found, "modelsEffortsCmd must be registered under modelsCmd")
}

// TestModelsHelp_CrossReferencesEfforts verifies `crush models --help`
// mentions the new command, per the task's requirement to extend (not
// restyle) the existing four-slot Long description.
func TestModelsHelp_CrossReferencesEfforts(t *testing.T) {
	assert.Contains(t, modelsCmd.Long, "crush models efforts --help")
}
