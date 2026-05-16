package cmd

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitModelEffort(t *testing.T) {
	cases := []struct {
		in           string
		wantModel    string
		wantEffort   string
	}{
		{"", "", ""},
		{"gpt-5", "gpt-5", ""},
		{"openai/gpt-5", "openai/gpt-5", ""},
		{"gpt-5@high", "gpt-5", "high"},
		{"openai/gpt-5@low", "openai/gpt-5", "low"},
		{"@only-effort", "", "only-effort"}, // edge: leading @
		{"weird@a@b", "weird@a", "b"},        // last @ wins
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotModel, gotEffort := splitModelEffort(tc.in)
			assert.Equal(t, tc.wantModel, gotModel, "model part")
			assert.Equal(t, tc.wantEffort, gotEffort, "effort part")
		})
	}
}

func TestMaskKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "-"},
		{"abcd", "****"},
		{"abc", "****"},
		{"sk-1234567890", "****7890"},
		{"$OPENAI_API_KEY", "$OPENAI_API_KEY"}, // unresolved env template: pass through
		{"${VAR:-default}", "${VAR:-default}"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, maskKey(tc.in))
		})
	}
}

func TestDash(t *testing.T) {
	assert.Equal(t, "-", dash(""))
	assert.Equal(t, "x", dash("x"))
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "hello", truncate("hello", 10))
	assert.Equal(t, "hello", truncate("hello", 5))
	assert.Equal(t, "hell…", truncate("hello world", 5))
	assert.Equal(t, "h", truncate("hello", 1))
}

func TestShort(t *testing.T) {
	assert.Equal(t, "", short(""))
	assert.Equal(t, "abc", short("abc"))
	assert.Equal(t, "01234567", short("0123456789abcdef"))
}

func TestScopeFromFlags(t *testing.T) {
	mk := func(global, local bool) *cobra.Command {
		c := &cobra.Command{}
		c.Flags().Bool("global", false, "")
		c.Flags().Bool("local", false, "")
		require.NoError(t, c.ParseFlags(scopeArgs(global, local)))
		return c
	}

	t.Run("default returns supplied default", func(t *testing.T) {
		s, err := scopeFromFlags(mk(false, false), config.ScopeWorkspace)
		require.NoError(t, err)
		assert.Equal(t, config.ScopeWorkspace, s)
	})
	t.Run("--global wins", func(t *testing.T) {
		s, err := scopeFromFlags(mk(true, false), config.ScopeWorkspace)
		require.NoError(t, err)
		assert.Equal(t, config.ScopeGlobal, s)
	})
	t.Run("--local wins", func(t *testing.T) {
		s, err := scopeFromFlags(mk(false, true), config.ScopeGlobal)
		require.NoError(t, err)
		assert.Equal(t, config.ScopeWorkspace, s)
	})
	t.Run("both set is an error", func(t *testing.T) {
		_, err := scopeFromFlags(mk(true, true), config.ScopeGlobal)
		assert.Error(t, err)
	})
}

func scopeArgs(global, local bool) []string {
	var args []string
	if global {
		args = append(args, "--global")
	}
	if local {
		args = append(args, "--local")
	}
	return args
}

func TestDefaultBaseURLFor(t *testing.T) {
	cases := []struct {
		typ  string
		want string
	}{
		{"anthropic", "https://api.anthropic.com/v1"},
		{"Anthropic", "https://api.anthropic.com/v1"}, // case-insensitive
		{"gemini", "https://generativelanguage.googleapis.com/v1beta"},
		{"openai", "https://api.openai.com/v1"},
		{"openai-compat", "https://api.openai.com/v1"}, // fallback
		{"random-thing", "https://api.openai.com/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			got := defaultBaseURLFor(config.ProviderConfig{Type: catwalk.Type(tc.typ)})
			assert.Equal(t, tc.want, got)
		})
	}
}
