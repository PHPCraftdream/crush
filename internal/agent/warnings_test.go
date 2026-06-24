package agent

import (
	"testing"

	"charm.land/fantasy"
)

// TestLogProviderWarnings_NoPanic verifies the warning logger tolerates a
// nil Tool, empty optional fields, and an empty slice without panicking.
func TestLogProviderWarnings_NoPanic(t *testing.T) {
	t.Parallel()

	// Empty slice: no-op.
	logProviderWarnings(nil)

	// Minimal warning: type + message only, nil Tool.
	logProviderWarnings([]fantasy.CallWarning{
		{Type: fantasy.CallWarningTypeOther, Message: "malformed tool call sanitized"},
	})

	// Warning with all optional fields empty and nil Tool.
	logProviderWarnings([]fantasy.CallWarning{
		{Type: fantasy.CallWarningTypeOther},
	})
}
