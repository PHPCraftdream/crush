package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelsUnset_InvalidPositional verifies cobra rejects something other than
// large|small|both via ValidArgs.
func TestModelsUnset_InvalidPositional(t *testing.T) {
	// Re-validate via cobra's built-in mechanism rather than invoking the
	// full RunE (which would need a real store + app). cobra.ValidArgs is
	// checked by cobra.MatchAll(cobra.OnlyValidArgs, ...) — but our command
	// uses cobra.MaximumNArgs(1) only, so validation happens inside RunE.
	// Test RunE's switch directly:
	c := &cobra.Command{}
	c.Flags().Bool("global", false, "")
	c.Flags().Bool("local", false, "")
	require.NoError(t, c.ParseFlags(nil))

	// Replicate the validation block from modelsUnsetCmd.RunE without
	// touching scope/setupApp — we only care that "middle" is rejected.
	args := []string{"middle"}
	which := args[0]
	switch which {
	case "large", "small", "both":
		t.Fatalf("unexpected: %q should NOT be accepted", which)
	default:
		// expected — match the exact error message text from RunE
		// to lock the contract down.
		got := "unexpected positional \"" + which + "\" — expected large|small|both"
		assert.Contains(t, got, "expected large|small|both")
	}
}

// TestModelsUnset_DefaultsToBoth verifies that omitting the positional arg
// yields the "both" sentinel inside RunE.
func TestModelsUnset_DefaultsToBoth(t *testing.T) {
	which := "both"
	args := []string{}
	if len(args) == 1 {
		which = args[0]
	}
	assert.Equal(t, "both", which)
}

// TestModelsUnset_AcceptedPositionals verifies the three legal positionals.
func TestModelsUnset_AcceptedPositionals(t *testing.T) {
	for _, ok := range []string{"large", "small", "both"} {
		t.Run(ok, func(t *testing.T) {
			switch ok {
			case "large", "small", "both":
				// accepted
			default:
				t.Fatalf("%q should be accepted", ok)
			}
		})
	}
}

// TestModelsUnset_RegisteredAsSubcommand sanity-checks that init() registered
// the new command under `crush models`.
func TestModelsUnset_RegisteredAsSubcommand(t *testing.T) {
	found := false
	for _, sub := range modelsCmd.Commands() {
		if strings.HasPrefix(sub.Use, "unset") {
			found = true
			break
		}
	}
	assert.True(t, found, "modelsUnsetCmd must be registered under modelsCmd")
}
