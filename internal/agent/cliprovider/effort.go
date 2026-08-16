package cliprovider

import (
	"log/slog"
	"slices"
)

// Reasoning-effort dispatch.
//
// A session stores ONE reasoning effort (session.LargeModelReasoningEffort, a
// persisted column) and agent.go:1916 puts it on the context for every model,
// with no per-provider guard. That value therefore reaches CLIs that have
// never heard of it, and CLIs whose model does not accept that particular
// level. Both must be handled here, at the spec boundary, because neither the
// agent nor the web UI can know a given binary's flag set.
//
// Measured 2026-08-16 against the installed binaries:
//
//	$ codex exec --effort high --help
//	error: unexpected argument '--effort' found
//	$ gemini --skip-trust --effort high -m gemini-3.5-flash -p "hi"
//	Unknown argument: effort
//	$ qwen --effort high -p "hi"
//	Unknown argument: effort
//
// So `--effort` is claude-only. codex takes the same idea as
// `-c model_reasoning_effort=<level>` (verified end to end for both `high`
// and `ultra`); gemini and qwen have no effort knob at all. qwen is the easy
// one to get wrong: it reuses claudePartParser/claudeParseUsageLine and so
// looks Claude-shaped, but it is a different binary with a different flag set.
//
// Levels are per-MODEL, not per-CLI, which is why EffortLevels lives on the
// spec rather than being derived from the binary name. From codex's embedded
// registry: gpt-5.6-sol/terra accept ultra, luna stops at max, gpt-5.5/5.4/5.2
// stop at xhigh. So passing a stale `max` (perfectly valid on Claude) to
// gpt-5.5 through the CORRECT flag would still fail — with a 400 from the API
// instead of an argv parse error. Validating the level is therefore part of
// the fix, not a refinement of it.

// Effort levels, kept as package vars so specs share one definition.
var (
	// claudeEffortLevels is what `claude --help` documents for --effort.
	claudeEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

	// codexEffortLevelsStandard covers the models whose registry entry stops
	// at xhigh (gpt-5.5, gpt-5.4, gpt-5.2, and the legacy slugs).
	codexEffortLevelsStandard = []string{"low", "medium", "high", "xhigh"}

	// codexEffortLevelsMax adds "max" (gpt-5.6-luna).
	codexEffortLevelsMax = []string{"low", "medium", "high", "xhigh", "max"}

	// codexEffortLevelsUltra adds "ultra" on top (gpt-5.6-sol, gpt-5.6-terra).
	//
	// "ultra" is NOT in the enum the API reports on a bad value
	// (none|minimal|low|medium|high|xhigh|max), yet `-c
	// model_reasoning_effort=ultra` completes a turn successfully against
	// these two - the CLI translates it client-side. The registry, not the
	// API error message, is the source of truth for what a model accepts.
	codexEffortLevelsUltra = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
)

// applyClaudeEffort sets --effort, replacing a value already placed there by
// BuildArgs so a UI toggle wins over the static default.
func applyClaudeEffort(args []string, effort string) []string {
	for i, a := range args {
		if a == "--effort" && i+1 < len(args) {
			args[i+1] = effort
			return args
		}
	}
	return append(args, "--effort", effort)
}

// applyCodexEffort passes the effort as an inline config override. codex has
// no --effort flag; `-c key=value` is the documented way to override anything
// from ~/.codex/config.toml for a single invocation, and
// model_reasoning_effort is the key it reads.
func applyCodexEffort(args []string, effort string) []string {
	return append(args, "-c", "model_reasoning_effort="+effort)
}

// applyEffort adds the requested reasoning effort to args for this spec.
//
// It is deliberately silent-and-safe rather than strict: an unusable effort is
// dropped with a log line instead of failing the run. The value arrives from a
// persisted session column that the operator may have set long ago against a
// different model, so refusing to run would turn a stale setting into a hard
// outage — the very failure this function exists to prevent.
func (s CLISpec) applyEffort(args []string, effort string) []string {
	if effort == "" {
		return args
	}

	// No effort knob on this CLI (gemini, qwen). Passing anything here is an
	// immediate "Unknown argument" and a dead run.
	if s.ApplyEffort == nil {
		slog.Debug(
			"cliprovider: dropping reasoning effort, this CLI has no effort option",
			"model", s.ModelID,
			"binary", s.Binary,
			"effort", effort,
		)
		return args
	}

	// Known knob, but this model does not accept this level. Dropping it
	// yields the model's own default, which is a working turn; forwarding it
	// yields a 400.
	if len(s.EffortLevels) > 0 && !slices.Contains(s.EffortLevels, effort) {
		slog.Warn(
			"cliprovider: dropping unsupported reasoning effort for this model, using its default",
			"model", s.ModelID,
			"binary", s.Binary,
			"effort", effort,
			"supported", s.EffortLevels,
		)
		return args
	}

	return s.ApplyEffort(args, effort)
}
