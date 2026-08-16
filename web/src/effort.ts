// Reasoning-effort capability, shared by every UI that offers the setting.
//
// This lived as private helpers inside ModelSelector.tsx. It moved here when a
// second surface (the Default-models modal) needed the same rules, because a
// second hand-written copy is exactly how a model that cannot take an effort
// ends up being offered one.
//
// That is not a cosmetic concern. Verified against the installed binaries on
// 2026-08-16:
//
//   gemini --effort high ...   -> Unknown argument: effort
//   qwen   --effort high ...   -> Unknown argument: effort
//   codex exec --effort high   -> error: unexpected argument '--effort' found
//
// The backend now drops an effort a CLI cannot accept
// (internal/agent/cliprovider/effort.go), but the UI must not offer it in the
// first place — and a value written at SYSTEM scope in the Default-models
// modal is inherited by every future session in every workspace, so a wrong
// one there is far wider-reaching than a per-session mistake.

// Claude CLI: `claude --help` documents low|medium|high|xhigh|max.
export const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;

// z.ai GLM-5.x only exposes High / Max natively (see docs.z.ai/devpack/
// latest-model and the MarkTechPost launch coverage). The chevron selector
// cycles through just these two; the backend mirrors them onto the provider's
// reasoning_effort field.
export const EFFORT_LEVELS_ZAI = ["high", "max"] as const;

// Returns true for any GLM-5.x model regardless of which provider key it lives
// under — users sometimes wire z.ai via a custom OpenAI-compat provider (id
// "z-ai" / "zhipu" / etc.), so matching the model id is the robust signal. The
// "[1m]" suffix variant (glm-5.2[1m]) is also covered. Older GLM-4.x families
// fall through to the binary thinking on/off in the coordinator and don't get
// the selector.
export function isZAIReasoningModel(_provider: string, model: string): boolean {
  return /^glm-5(\.|-|\[|$)/i.test(model);
}

// Returns true if the model is a CLI Claude model (supports reasoning_effort).
export function isCLIClaudeModel(provider: string, model: string): boolean {
  return provider === "local-cli" && (model.startsWith("cli-claude-") || model.startsWith("cli-npx-claude-"));
}

// effortLevelsFor returns the levels this model accepts, or null when it has
// no reasoning-effort knob at all.
//
// null is deliberately distinct from an empty array: callers must hide the
// control entirely rather than render an empty dropdown, and must not persist
// an effort for such a model.
//
// Codex is NOT listed yet even though its CLI does accept an effort
// (`-c model_reasoning_effort=`), because its levels are per-model — codex's
// own registry stops gpt-5.5 at "xhigh" while gpt-5.6-sol accepts "ultra" —
// and the frontend has no source for that table. Hardcoding a third copy of
// per-model levels here is what this module exists to prevent; wire it through
// from the backend spec when codex effort is exposed.
export function effortLevelsFor(provider: string, model: string): readonly string[] | null {
  if (isZAIReasoningModel(provider, model)) return EFFORT_LEVELS_ZAI;
  if (isCLIClaudeModel(provider, model)) return EFFORT_LEVELS;
  return null;
}

// supportsEffort is the boolean form, for callers that only need to decide
// whether to render a control.
export function supportsEffort(provider: string, model: string): boolean {
  return effortLevelsFor(provider, model) !== null;
}

// defaultEffortFor is the level to show when the session has none stored.
// Claude CLI keeps the legacy "medium"; z.ai GLM-5.x defaults to "high"
// (Max is opt-in for heavy work — the same wording z.ai uses).
export function defaultEffortFor(provider: string, model: string): string {
  return isZAIReasoningModel(provider, model) ? "high" : "medium";
}

// clampEffort maps a stored effort onto something this model accepts.
//
// Returns null when the model takes no effort at all, which callers must treat
// as "clear the stored value", not "keep it": a session that moves from Claude
// to gemini leaves behind an effort the new model cannot use, and that stale
// value is what used to kill the run outright.
export function clampEffort(provider: string, model: string, stored: string): string | null {
  const levels = effortLevelsFor(provider, model);
  if (levels === null) return null;
  if (stored && levels.includes(stored)) return stored;
  const fallback = defaultEffortFor(provider, model);
  return levels.includes(fallback) ? fallback : levels[0];
}
