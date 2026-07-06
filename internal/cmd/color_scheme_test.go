package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveColorScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		flagValue string
		envValue  string
		want      string
		wantDark  bool
	}{
		// --- empty inputs default to auto ---
		{name: "both empty", flagValue: "", envValue: "", want: "auto", wantDark: false},
		{name: "whitespace only flag and env", flagValue: "   ", envValue: "\t", want: "auto", wantDark: false},

		// --- flag precedence over env ---
		{name: "flag light beats env dark", flagValue: "light", envValue: "dark", want: "light", wantDark: false},
		{name: "flag dark beats env light", flagValue: "dark", envValue: "light", want: "dark", wantDark: true},
		{name: "flag auto beats env dark", flagValue: "auto", envValue: "dark", want: "auto", wantDark: false},

		// --- env used when flag empty ---
		{name: "env light when flag empty", flagValue: "", envValue: "light", want: "light", wantDark: false},
		{name: "env dark when flag empty", flagValue: "", envValue: "dark", want: "dark", wantDark: true},
		{name: "env auto when flag empty", flagValue: "", envValue: "auto", want: "auto", wantDark: false},

		// --- case insensitivity ---
		{name: "flag LIGHT upper", flagValue: "LIGHT", envValue: "", want: "light", wantDark: false},
		{name: "flag Dark mixed case", flagValue: "Dark", envValue: "", want: "dark", wantDark: true},
		{name: "flag AUTO upper", flagValue: "AUTO", envValue: "", want: "auto", wantDark: false},
		{name: "env with surrounding spaces", flagValue: "", envValue: "  dark  ", want: "dark", wantDark: true},

		// --- invalid inputs fall back to auto, never error ---
		{name: "invalid flag falls back", flagValue: "solarized", envValue: "", want: "auto", wantDark: false},
		{name: "invalid env falls back", flagValue: "", envValue: "nope", want: "auto", wantDark: false},
		{name: "invalid flag beats valid env (still auto)", flagValue: "banana", envValue: "dark", want: "auto", wantDark: false},
		{name: "empty string literal flag", flagValue: "", envValue: "high-contrast", want: "auto", wantDark: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveColorScheme(tc.flagValue, tc.envValue)
			require.Equal(t, tc.want, got, "resolved value mismatch")
			require.Equal(t, tc.wantDark, isDarkColorScheme(got), "isDark mismatch")
		})
	}
}

func TestResolveColorSchemeValidatesKnownConstants(t *testing.T) {
	t.Parallel()

	// Guard against accidental drift between the constants and the switch.
	for _, v := range []string{"light", "dark", ColorSchemeAuto} {
		require.Equal(t, v, resolveColorScheme(v, ""))
	}
}

func TestColorSchemeFlagFromArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "absent", args: []string{}, want: ""},
		{name: "absent among other flags", args: []string{"--debug", "--port", "8080"}, want: ""},
		{name: "space form", args: []string{"--color-scheme", "light"}, want: "light"},
		{name: "equals form", args: []string{"--color-scheme=dark"}, want: "dark"},
		{name: "single dash equals", args: []string{"-color-scheme=auto"}, want: "auto"},
		{name: "after other flags space form", args: []string{"--debug", "--color-scheme", "dark"}, want: "dark"},
		{name: "after other flags equals form", args: []string{"-p", "8080", "--color-scheme=light"}, want: "light"},
		{name: "no value after space form -> empty", args: []string{"--color-scheme"}, want: ""},
		{name: "next token is a flag -> empty", args: []string{"--color-scheme", "--debug"}, want: ""},
		{name: "stops at -- sentinel", args: []string{"--", "--color-scheme", "light"}, want: ""},
		{name: "before subcommand", args: []string{"--color-scheme=light", "run", "hi"}, want: "light"},
		{name: "preserves mixed case value", args: []string{"--color-scheme", "Dark"}, want: "Dark"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, colorSchemeFlagFromArgs(tc.args))
		})
	}
}