package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStubResolve = errors.New("stub resolve")

func noopResolve(s string) (string, string, error) {
	return "", "", errStubResolve
}

func TestParseAtom_AnthropicWithLevel(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high"})()
	sm, err := parseAtom("opus-high")
	require.NoError(t, err)
	assert.Equal(t, "local-cli", sm.Provider)
	// `opus` is now an alias for the latest pinned Opus version (4.8).
	// Earlier atoms pointed at the legacy "cli-claude-opus" alias entry;
	// we kept the registry pointing at the pinned id so newly-created
	// sessions track the freshest generation.
	assert.Equal(t, "cli-claude-opus-4-8", sm.Model)
	assert.Equal(t, "high", sm.ReasoningEffort)
}

func TestParseAtom_AnthropicMissingLevel(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high"})()
	_, err := parseAtom("opus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires explicit level")
}

func TestParseAtom_AnthropicInvalidLevel(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high"})()
	_, err := parseAtom("opus-blah")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid level")
}

func TestParseAtom_ZAINoLevel(t *testing.T) {
	sm, err := parseAtom("glm5_1")
	require.NoError(t, err)
	assert.Equal(t, "zai", sm.Provider)
	assert.Equal(t, "glm-5.1", sm.Model)
	assert.Empty(t, sm.ReasoningEffort)
}

func TestParseAtom_ZAIRejectsLevel(t *testing.T) {
	_, err := parseAtom("glm5_1-low")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "does not support effort")
}

func TestParseAtom_LongestMatchWins(t *testing.T) {
	// glm5, glm5_1, glm5_turbo all share the "glm5" prefix; verify the
	// parser picks the longest match rather than greedily stopping at the
	// shortest.
	sm, err := parseAtom("glm5_turbo")
	require.NoError(t, err)
	assert.Equal(t, "glm-5-turbo", sm.Model)

	sm2, err := parseAtom("glm5_1")
	require.NoError(t, err)
	assert.Equal(t, "glm-5.1", sm2.Model)

	sm3, err := parseAtom("glm5")
	require.NoError(t, err)
	assert.Equal(t, "glm-5", sm3.Model)
}

func TestParseAtom_UnknownAtom(t *testing.T) {
	_, err := parseAtom("totally-fake-model")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a recognized atom")
}

func TestParseAtomOrRaw_FallsBackToProviderSlashModel(t *testing.T) {
	resolve := func(s string) (string, string, error) {
		assert.Equal(t, "openai/gpt-5", s)
		return "openai", "gpt-5", nil
	}
	sm, err := parseAtomOrRaw("openai/gpt-5@high", resolve)
	require.NoError(t, err)
	assert.Equal(t, "openai", sm.Provider)
	assert.Equal(t, "gpt-5", sm.Model)
	assert.Equal(t, "high", sm.ReasoningEffort)
}

func TestParseAtomOrRaw_AtomWinsOverFallback(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "high"})()
	called := false
	resolve := func(s string) (string, string, error) {
		called = true
		return "", "", nil
	}
	sm, err := parseAtomOrRaw("opus-high", resolve)
	require.NoError(t, err)
	assert.False(t, called, "resolver must NOT be called when atom matches")
	assert.Equal(t, "local-cli", sm.Provider)
	assert.Equal(t, "cli-claude-opus-4-8", sm.Model)
}

func TestParseAtomOrRaw_UnknownAtomAndNoSlash(t *testing.T) {
	_, err := parseAtomOrRaw("nonsense", noopResolve)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known atom or provider/model")
}

func TestLookupAtomForModel(t *testing.T) {
	k := lookupAtomForModel(config.SelectedModel{Provider: "local-cli", Model: "cli-claude-opus-4-8"})
	// Both "opus" (alias to latest) and "opus48" (explicit pin) resolve to
	// the same underlying model id; lookupAtomForModel returns whichever
	// key the registry walk reaches first, so accept either.
	assert.Contains(t, []string{"opus", "opus48"}, k)
	k2 := lookupAtomForModel(config.SelectedModel{Provider: "zai", Model: "glm-5-turbo"})
	assert.Equal(t, "glm5_turbo", k2)
	k3 := lookupAtomForModel(config.SelectedModel{Provider: "openai", Model: "gpt-5"})
	assert.Empty(t, k3, "non-atom model returns empty string")
}

// (splitModelEffort is covered by helpers_test.go — don't duplicate.)

func TestParseShortCode_Valid(t *testing.T) {
	cases := []struct {
		code   string
		model  string
		effort string
	}{
		{"o48h", "cli-claude-opus-4-8", "high"},
		{"o48xx", "cli-claude-opus-4-8", "max"},
		{"o47h", "cli-claude-opus-4-7", "high"},
		{"o47xx", "cli-claude-opus-4-7", "max"},
		{"o47x", "cli-claude-opus-4-7", "xhigh"},
		{"o46xx", "cli-claude-opus-4-6", "max"},
		{"s46h", "cli-claude-sonnet", "high"},
		{"s45h", "cli-claude-sonnet", "high"},
		{"h45l", "cli-claude-haiku", "low"},
		{"oh", "cli-claude-opus-4-8", "high"},
		{"sl", "cli-claude-sonnet", "low"},
		{"hm", "cli-claude-haiku", "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			sm, ok := parseShortCode(tc.code)
			require.True(t, ok, "expected ok for %s", tc.code)
			assert.Equal(t, "local-cli", sm.Provider)
			assert.Equal(t, tc.model, sm.Model)
			assert.Equal(t, tc.effort, sm.ReasoningEffort)
		})
	}
}

func TestParseShortCode_Invalid(t *testing.T) {
	invalid := []string{"o47-3", "o47-0", "s45xx", "h45x", "x47h", "o99h", "", "opus-high"}
	for _, code := range invalid {
		_, ok := parseShortCode(code)
		assert.False(t, ok, "expected not-ok for %q", code)
	}
}

func TestParseAtom_ShortCodeRoundtrip(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high", "xhigh", "max"})()
	sm, err := parseAtom("o47x")
	require.NoError(t, err)
	assert.Equal(t, "local-cli", sm.Provider)
	assert.Equal(t, "cli-claude-opus-4-7", sm.Model)
	assert.Equal(t, "xhigh", sm.ReasoningEffort)
}
