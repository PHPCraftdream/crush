package cliprovider

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Task #471 — reasoning effort must be dispatched per CLI, not appended blindly.
//
// The old code appended `--effort <level>` to whatever binary was launched.
// Only claude has that flag; verified against the installed binaries:
//
//	codex exec --effort high   -> error: unexpected argument '--effort' found
//	gemini --effort high ...   -> Unknown argument: effort
//	qwen --effort high ...     -> Unknown argument: effort
//
// That is 9 of the 19 registered specs (gemini 2 + qwen 1 + codex 6) dying on
// the first turn as soon as a session carries an effort — and the session's
// effort is a PERSISTED column that agent.go puts on the context for every
// model, so "set an effort on Claude, then switch this session to codex" is a
// live path, not a hypothetical.
//
// REVERT CHECK: replace the m.spec.applyEffort call in Stream with the old
// unconditional `append(args, "--effort", effort)` and
// TestEffort_NotSentToCLIsWithoutTheKnob fails for gemini, qwen and codex.

// effortArgs runs a spec's BuildArgs then applies the effort the way Stream
// does, returning the final argv.
func effortArgs(spec CLISpec, effort string) []string {
	return spec.applyEffort(spec.BuildArgs(false), effort)
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestEffort_NotSentToCLIsWithoutTheKnob is the core regression: a CLI with no
// effort option must receive no trace of one, whatever the session stored.
func TestEffort_NotSentToCLIsWithoutTheKnob(t *testing.T) {
	for _, spec := range All {
		if spec.ApplyEffort != nil {
			continue // has a knob; covered by the tests below
		}
		t.Run(spec.ModelID, func(t *testing.T) {
			before := spec.BuildArgs(false)
			after := effortArgs(spec, "high")

			require.Equal(t, before, after,
				"%s (%s) has no effort option; argv must be untouched", spec.ModelID, spec.Binary)
			require.False(t, hasFlag(after, "--effort"),
				"%s rejects --effort with 'Unknown argument'", spec.Binary)
			require.NotContains(t, strings.Join(after, " "), "model_reasoning_effort",
				"%s is not codex; it must not receive codex's config override either", spec.Binary)
		})
	}
}

// TestEffort_GeminiAndQwenAreTheOnesWithoutIt pins WHICH specs are expected to
// have no knob, so a future spec that silently forgets to set ApplyEffort is
// noticed here rather than by a user whose runs stop working.
func TestEffort_GeminiAndQwenAreTheOnesWithoutIt(t *testing.T) {
	for _, spec := range All {
		switch spec.Binary {
		case "gemini", "qwen":
			require.Nil(t, spec.ApplyEffort,
				"%s has no reasoning-effort option; ApplyEffort must stay nil", spec.Binary)
		case "claude", "codex":
			require.NotNil(t, spec.ApplyEffort,
				"%s accepts a reasoning effort; spec %s must declare how", spec.Binary, spec.ModelID)
			require.NotEmpty(t, spec.EffortLevels,
				"%s must declare which levels it accepts, or a stale level reaches the provider", spec.ModelID)
		default:
			t.Fatalf("unclassified CLI binary %q in spec %q — decide whether it takes an effort", spec.Binary, spec.ModelID)
		}
	}
}

func TestEffort_ClaudeUsesTheFlag(t *testing.T) {
	spec := specByID(t, "cli-claude-sonnet")
	args := effortArgs(spec, "xhigh")

	idx := -1
	for i, a := range args {
		if a == "--effort" {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "claude takes --effort")
	require.Equal(t, "xhigh", args[idx+1])
}

// TestEffort_ClaudeReplacesRatherThanDuplicates guards the behaviour the old
// code had and which must survive: a value already placed by BuildArgs is
// overwritten, not appended a second time (two --effort flags would make the
// CLI use whichever it parses last, silently ignoring the UI toggle).
func TestEffort_ClaudeReplacesRatherThanDuplicates(t *testing.T) {
	spec := specByID(t, "cli-claude-sonnet")
	spec.BuildArgs = func(bool) []string {
		return []string{"--model", "sonnet", "--effort", "low"}
	}

	args := effortArgs(spec, "max")

	count := 0
	for _, a := range args {
		if a == "--effort" {
			count++
		}
	}
	require.Equal(t, 1, count, "must replace the existing --effort, not append a duplicate")
	require.Contains(t, strings.Join(args, " "), "--effort max")
}

// TestEffort_CodexUsesConfigOverride pins codex's different mechanism.
// Verified working end to end against codex 0.147.0 for both high and ultra.
func TestEffort_CodexUsesConfigOverride(t *testing.T) {
	spec := specByID(t, "cli-codex")
	args := effortArgs(spec, "high")

	require.False(t, hasFlag(args, "--effort"), "codex rejects --effort outright")
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "-c model_reasoning_effort=high",
		"codex takes the effort as an inline config override")
}

// TestEffort_UnsupportedLevelIsDroppedNotForwarded is the second half of the
// fix. Levels are per-MODEL: codex's registry stops gpt-5.5 at xhigh while
// gpt-5.6-sol accepts ultra, and claude goes to max. A session carrying "max"
// from a Claude model would otherwise reach a codex model through the CORRECT
// flag and fail with a 400 from the API — a valid-looking argv that still
// breaks the run.
func TestEffort_UnsupportedLevelIsDroppedNotForwarded(t *testing.T) {
	spec := specByID(t, "cli-codex")
	require.NotContains(t, spec.EffortLevels, "max",
		"precondition: this codex spec is one that stops below max")

	before := spec.BuildArgs(false)
	after := effortArgs(spec, "max")

	require.Equal(t, before, after,
		"an effort this model does not accept must be dropped so the model's own default applies")
	require.NotContains(t, strings.Join(after, " "), "model_reasoning_effort")
}

func TestEffort_EmptyEffortIsANoOp(t *testing.T) {
	for _, id := range []string{"cli-claude-sonnet", "cli-codex", "cli-gemini-flash"} {
		spec := specByID(t, id)
		require.Equal(t, spec.BuildArgs(false), effortArgs(spec, ""),
			"%s: no effort set means no change", id)
	}
}
