package cliprovider

import (
	"regexp"
	"strings"
	"testing"
)

// Task #470 — Claude 5 spec additions and the stale-display-name regression.
//
// Two distinct properties are locked here.
//
// 1. The pinned Claude 5 entries pass the exact argument the CLI expects.
//    The `[1m]` suffix is a real context-window switch, not decoration:
//    measured against claude 2.1.197, `--model claude-opus-5` reports
//    contextWindow=200_000 while `--model claude-opus-5[1m]` reports
//    1_000_000. Dropping or mangling the suffix silently downgrades the
//    model to a fifth of the advertised window, which our ContextWindow of
//    1_000_000 would then overstate — the same class of bug as the codex
//    400k-vs-272k mismatch. Note we deliberately keep the brackets OUT of
//    ModelID (they would end up inside `provider/model` strings in config,
//    atoms and DB rows) and only ever pass them as the CLI argument.
//
// 2. Alias-backed specs must not name a version in their display name.
//    `cli-claude-sonnet` passes the moving alias `sonnet`, which the CLI
//    resolves to whatever it currently defaults to — measured 2026-08-16
//    that is claude-sonnet-5, while the entry was labelled "Claude Sonnet
//    4.6 (CLI)". The UI was naming the wrong model. A version number is
//    only honest on a spec that pins an explicit model id.

// modelArgOf returns the value passed after --model by a spec's BuildArgs.
func modelArgOf(t *testing.T, spec CLISpec) string {
	t.Helper()
	args := spec.BuildArgs(false)
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("spec %q builds no --model argument: %v", spec.ModelID, args)
	return ""
}

func specByID(t *testing.T, id string) CLISpec {
	t.Helper()
	for _, s := range All {
		if s.ModelID == id {
			return s
		}
	}
	t.Fatalf("spec %q not registered in All", id)
	return CLISpec{}
}

func TestClaude5PinnedSpecsPassExactModelArgument(t *testing.T) {
	for _, tc := range []struct {
		modelID string
		wantArg string
	}{
		{"cli-claude-opus-5-1m", "claude-opus-5[1m]"},
		{"cli-claude-sonnet-5-1m", "claude-sonnet-5[1m]"},
		{"cli-claude-fable-5", "claude-fable-5"},
	} {
		spec := specByID(t, tc.modelID)
		if got := modelArgOf(t, spec); got != tc.wantArg {
			t.Errorf("%s passes --model %q, want %q", tc.modelID, got, tc.wantArg)
		}
		// The bracketed form must never leak into the ModelID itself.
		if strings.ContainsAny(tc.modelID, "[]") {
			t.Errorf("ModelID %q contains brackets; keep them in the CLI argument only", tc.modelID)
		}
	}
}

// versionInName matches a version number in a display name, e.g. "4.6",
// "Opus 5", "4-8". Deliberately loose — a false positive here is a nudge to
// pin the model instead of naming a version on a moving alias.
var versionInName = regexp.MustCompile(`\d`)

func TestAliasBackedSpecsDoNotClaimAVersion(t *testing.T) {
	// Specs whose --model argument is one of the CLI's moving aliases.
	aliases := map[string]bool{
		"opus": true, "sonnet": true, "haiku": true, "fable": true,
		"opusplan": true, "default": true, "mythos": true,
	}
	for _, spec := range All {
		if spec.Binary != "claude" {
			continue
		}
		arg := modelArgOf(t, spec)
		if !aliases[arg] {
			continue // pinned id — a version in the name is accurate
		}
		if versionInName.MatchString(spec.ModelName) {
			t.Errorf(
				"spec %q passes moving alias %q but its display name %q names a version; "+
					"the CLI decides which model that alias resolves to, so the label goes stale silently",
				spec.ModelID, arg, spec.ModelName,
			)
		}
	}
}
