package cmd

import (
	"log/slog"
	"strings"
)

// ColorSchemeAuto leaves fang's built-in light/dark auto-detection in charge
// (the historical behaviour when no override is configured).
const ColorSchemeAuto = "auto"

// resolveColorScheme resolves the effective color-scheme value from an
// explicit flag value and the CRUSH_COLOR_SCHEME environment variable.
//
// Precedence: a non-empty flagValue wins over envValue. The result is one
// of "light", "dark", or "auto" (case-insensitive on input). Anything that
// is not a recognised value falls back to "auto" with a warning, so a
// typo can never take the CLI down — it just reverts to today's behaviour.
//
// This function is pure (no I/O) so it can be unit-tested without a real
// terminal. slog.Warn is the only side-effect, and only on invalid input.
func resolveColorScheme(flagValue, envValue string) string {
	raw := strings.TrimSpace(flagValue)
	if raw == "" {
		raw = strings.TrimSpace(envValue)
	}
	if raw == "" {
		return ColorSchemeAuto
	}

	switch strings.ToLower(raw) {
	case "light", "dark", ColorSchemeAuto:
		return strings.ToLower(raw)
	default:
		slog.Warn(
			"Invalid color scheme value; falling back to auto",
			"value", raw,
			"valid_values", []string{"light", "dark", "auto"},
		)
		return ColorSchemeAuto
	}
}

// isDarkColorScheme reports whether a resolved color-scheme value should
// render for a dark background. "auto" is intentionally false here: callers
// must check for "auto" separately and skip forcing a scheme in that case.
func isDarkColorScheme(resolved string) bool {
	return resolved == "dark"
}
