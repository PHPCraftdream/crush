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

// ── Codex registry alignment (task #477) ─────────────────────────────────────

// TestCodexSpecs_ContextWindowMatchesRegistry pins the number against codex's
// OWN embedded model registry (codex-cli 0.147.0), which reports 272_000 for
// every model it serves.
//
// The specs previously claimed 400_000. ContextWindow feeds the
// auto-summarization threshold, so a 48% overstatement let conversations run
// well past the real limit before compaction triggered — the same class of
// silent failure as the PromptTokens understatement fixed alongside it.
func TestCodexSpecs_ContextWindowMatchesRegistry(t *testing.T) {
	seen := 0
	for _, spec := range All {
		if spec.Binary != "codex" {
			continue
		}
		seen++
		if spec.ContextWindow != 272_000 {
			t.Errorf("%s declares ContextWindow %d, registry says 272000",
				spec.ModelID, spec.ContextWindow)
		}
	}
	if seen == 0 {
		t.Fatal("no codex specs found; this test would pass vacuously")
	}
}

// TestCodexSpecs_EffortLevelsArePerModel guards the distinction that makes the
// EffortLevels field necessary at all: the ceiling differs by model, so one
// shared list would either forbid a level sol accepts or forward one gpt-5.5
// rejects.
func TestCodexSpecs_EffortLevelsArePerModel(t *testing.T) {
	want := map[string]string{
		"cli-codex-sol":          "ultra", // registry: sol/terra go to ultra
		"cli-codex-terra":        "ultra",
		"cli-codex-luna":         "max", // luna stops at max
		"cli-codex-gpt-5-5":      "xhigh",
		"cli-codex-gpt-5-4":      "xhigh",
		"cli-codex-gpt-5-2-base": "xhigh",
	}
	for id, ceiling := range want {
		spec := specByID(t, id)
		if len(spec.EffortLevels) == 0 {
			t.Fatalf("%s declares no effort levels", id)
		}
		got := spec.EffortLevels[len(spec.EffortLevels)-1]
		if got != ceiling {
			t.Errorf("%s tops out at %q, registry says %q", id, got, ceiling)
		}
	}

	// And the ceilings must genuinely differ, or the per-model plumbing is
	// pointless and this test is decoration.
	sol := specByID(t, "cli-codex-sol")
	base := specByID(t, "cli-codex-gpt-5-2-base")
	if len(sol.EffortLevels) == len(base.EffortLevels) {
		t.Error("sol and gpt-5.2 must not share a level list; that is why EffortLevels is per-spec")
	}
}

// TestCodexSpecs_RetiredSlugsSaySo keeps the four slugs that vanished from the
// registry visible but honest. They are kept, not deleted, so existing session
// rows referencing them do not dangle — but codex silently falls back to
// generic metadata for them ("this can degrade performance"), and an entry
// that degrades quietly is worse than one that refuses loudly.
func TestCodexSpecs_RetiredSlugsSaySo(t *testing.T) {
	for _, id := range []string{"cli-codex", "cli-codex-gpt-5-2", "cli-codex-max", "cli-codex-mini"} {
		spec := specByID(t, id)
		if !strings.Contains(spec.ModelName, "unsupported") {
			t.Errorf("%s is absent from codex's registry; its display name %q must say so",
				id, spec.ModelName)
		}
	}
}

// TestCodexSpecs_CurrentSlugsArePresent pins the models verified to work.
// Each was pinged against the real CLI (turn.completed) before being added.
func TestCodexSpecs_CurrentSlugsArePresent(t *testing.T) {
	for _, tc := range []struct{ id, arg string }{
		{"cli-codex-sol", "gpt-5.6-sol"},
		{"cli-codex-terra", "gpt-5.6-terra"},
		{"cli-codex-luna", "gpt-5.6-luna"},
		{"cli-codex-gpt-5-5", "gpt-5.5"},
	} {
		spec := specByID(t, tc.id)
		args := strings.Join(spec.BuildArgs(false), " ")
		if !strings.Contains(args, tc.arg) {
			t.Errorf("%s must pass -m %s, got: %s", tc.id, tc.arg, args)
		}
		if strings.Contains(spec.ModelName, "unsupported") {
			t.Errorf("%s is current; it must not be labelled unsupported", tc.id)
		}
	}
}

// ── Gemini specs (task #478) ─────────────────────────────────────────────────

// TestGeminiSpecs_PassExactModelArgument pins the ids against the CLI's own
// VALID_GEMINI_MODELS set. Both additions were pinged OK through crush.
func TestGeminiSpecs_PassExactModelArgument(t *testing.T) {
	for _, tc := range []struct{ id, arg string }{
		{"cli-gemini-flash", "gemini-3-flash"},
		{"cli-gemini-flash-35", "gemini-3.5-flash"},
		{"cli-gemini-flash-lite", "gemini-3.1-flash-lite"},
		{"cli-gemini-pro", "gemini-3.1-pro-preview"},
	} {
		spec := specByID(t, tc.id)
		args := strings.Join(spec.BuildArgs(false), " ")
		if !strings.Contains(args, tc.arg) {
			t.Errorf("%s must pass -m %s, got: %s", tc.id, tc.arg, args)
		}
	}
}

// TestGeminiFlashAliasDoesNotClaimAVersion covers a stale label the way the
// claude test does. cli-gemini-flash pins gemini-3-flash, but that id now
// REDIRECTS: the CLI's response reports the resolved model as
// gemini-3.5-flash, so the entry ran 3.5 while advertising 3. Pin 3.5
// explicitly via cli-gemini-flash-35 instead.
func TestGeminiFlashAliasDoesNotClaimAVersion(t *testing.T) {
	spec := specByID(t, "cli-gemini-flash")
	if versionInName.MatchString(spec.ModelName) {
		t.Errorf("cli-gemini-flash resolves server-side to a different version; "+
			"its name %q must not pin one", spec.ModelName)
	}
}

// TestNoMythosSpec records a deliberate absence. claude-mythos-5 and the
// `mythos` alias both exist in the CLI but return HTTP 404 model_not_found on
// this account, so exposing them would offer a model that cannot run.
func TestNoMythosSpec(t *testing.T) {
	for _, spec := range All {
		if strings.Contains(spec.ModelID, "mythos") {
			t.Errorf("spec %q exposes mythos, which 404s; re-add only after a ping resolves to it", spec.ModelID)
		}
	}
}
