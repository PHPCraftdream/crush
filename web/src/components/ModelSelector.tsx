import { useState, useRef, useEffect, useMemo } from "react";
import { useStore } from "@nanostores/react";
import { BrainCircuit, Zap, ChevronLeft, ChevronRight } from "lucide-react";
import {
  $config,
  $recentLargeModels,
  $recentSmallModels,
  trackModelUsage,
  removeRecentModel,
  getDefaultModelKey,
  setSessionModels,
  setSessionReasoningEffort,
  setProviderKey,
  removeProviderKey,
} from "../store";
import type { ConfigPayload, Session } from "../types";

// Effort levels in cycle order: left arrow decrements, right arrow increments
// Five effort tiers — same names the Claude CLI accepts on `--effort`
// (and what `defaultEffortLevels` in internal/cmd/models_effort.go ships).
// Labels mirror our short-code convention: oh / ox / oxx → high / xhigh / max.
const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;
const EFFORT_LABELS: Record<string, string> = {
  low: "L",
  medium: "M",
  high: "H",
  xhigh: "X",
  max: "XX",
};

// z.ai GLM-5.x only exposes High / Max natively (see docs.z.ai/devpack/
// latest-model and the MarkTechPost launch coverage). The chevron selector
// cycles through just these two; the backend mirrors them onto the
// provider's reasoning_effort field.
const EFFORT_LEVELS_ZAI = ["high", "max"] as const;

// Returns true for any GLM-5.x model regardless of which provider key
// it lives under — users sometimes wire z.ai via a custom OpenAI-compat
// provider (id "z-ai" / "zhipu" / etc.), so matching the model id is
// the robust signal. The "[1m]" suffix variant (glm-5.2[1m]) is also
// covered. Older GLM-4.x families fall through to the binary thinking
// on/off in the coordinator and don't get the selector.
function isZAIReasoningModel(_provider: string, model: string): boolean {
  return /^glm-5(\.|-|\[|$)/i.test(model);
}

// Returns true if the model is a CLI Claude model (supports reasoning_effort)
function isCLIClaudeModel(provider: string, model: string): boolean {
  return provider === "local-cli" && (model.startsWith("cli-claude-") || model.startsWith("cli-npx-claude-"));
}

// ── Types ─────────────────────────────────────────────────────────────────────

export interface ModelItem {
  key: string;
  providerID: string;
  providerName: string;
  providerType: string;
  modelID: string;
  name: string;
  contextWindow: number;
  enabled: boolean; // provider has an API key configured
}

export interface ProviderGroup {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  models: ModelItem[];
}

// ── Helper functions ──────────────────────────────────────────────────────────

// Builds a list of provider groups, each with their models
export function buildProviderGroups(config: ConfigPayload | null): ProviderGroup[] {
  const groups: ProviderGroup[] = [];
  const seen = new Set<string>();
  for (const [providerID, p] of Object.entries(config?.providers ?? {})) {
    const enabled = p.enabled ?? false;
    const providerName = p.name || providerID;
    const providerType = p.type ?? "";
    const models: ModelItem[] = [];
    for (const m of (p.models ?? [])) {
      const key = `${providerID}:::${m.id}`;
      if (!seen.has(key)) {
        seen.add(key);
        models.push({ key, providerID, providerName, providerType, modelID: m.id, name: m.name || m.id, contextWindow: m.contextWindow ?? 0, enabled });
      }
    }
    if (models.length > 0) {
      groups.push({ id: providerID, name: providerName, type: providerType, enabled, models });
    }
  }
  // Sort: enabled providers first, then alphabetically
  groups.sort((a, b) => {
    if (a.enabled !== b.enabled) return a.enabled ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
  return groups;
}

// Builds a flat list of all models from all providers
export function buildModelList(config: ConfigPayload | null): ModelItem[] {
  return buildProviderGroups(config).flatMap(g => g.models);
}

// ── APIKeyForm ────────────────────────────────────────────────────────────────

// APIKeyForm is the inline form shown when user clicks a disabled model or edits a provider key.
function APIKeyForm({ providerID, providerName, onDone, onCancel }: {
  providerID: string;
  providerName: string;
  onDone: () => void;
  onCancel: () => void;
}) {
  const [key, setKey] = useState("");
  const [saving, setSaving] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => { inputRef.current?.focus(); }, []);

  function save() {
    const trimmed = key.trim();
    if (!trimmed) return;
    setSaving(true);
    setProviderKey(providerID, trimmed);
    // The server will broadcast an updated config — onDone will close the form
    onDone();
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Enter") save();
    if (e.key === "Escape") onCancel();
  }

  return (
    <div className="p-3 border-b border-surface/40 bg-base-overlay/50" data-test-id="model-api-key-form" onClick={e => e.stopPropagation()}>
      <div className="text-xs font-semibold text-text mb-1.5">{providerName} — Enter API key</div>
      <div className="flex gap-2">
        <input
          ref={inputRef}
          type="password"
          value={key}
          onChange={e => setKey(e.target.value)}
          onKeyDown={onKey}
          placeholder="sk-…"
          className="flex-1 bg-canvas border border-surface rounded-lg px-2.5 py-1.5 text-sm text-text outline-none focus:border-accent transition-colors placeholder:text-text-subtle font-mono"
        />
        <button
          onClick={save}
          disabled={!key.trim() || saving}
          className="px-3 py-1.5 bg-accent-fill text-white/90 text-xs font-medium rounded-lg disabled:opacity-40 hover:opacity-90 transition-opacity"
        >
          Save
        </button>
        <button
          onClick={onCancel}
          className="px-3 py-1.5 text-text-subtle text-xs rounded-lg hover:bg-base-subtle transition-colors"
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

// ── ModelRow ──────────────────────────────────────────────────────────────────

function ModelRow({ model, isSelected, onSelect }: { model: ModelItem, isSelected: boolean, onSelect: (m: ModelItem) => void }) {
  const disabled = !model.enabled;
  const isCLI = model.providerType === "cli";
  const canAddKey = disabled && !isCLI;
  return (
    <button
      onClick={() => onSelect(model)}
      data-test-id={`model-item-${model.key}`}
      className={`w-full text-left px-3 py-2 transition-colors border-b border-surface/30 last:border-0 ${
        disabled ? "opacity-50 hover:bg-base-overlay" : isSelected ? "bg-accent/5 hover:bg-accent/8" : "hover:bg-base-overlay"
      }`}
      title={canAddKey ? `${model.providerName}: click to add API key` : undefined}
    >
      <div className={`text-sm font-medium truncate ${isSelected ? "text-accent" : disabled ? "text-text-subtle" : "text-text"}`}>
        {model.name}
        {canAddKey && <span className="ml-2 text-[10px] text-accent/70 font-normal">+ add key</span>}
      </div>
    </button>
  );
}

// ── ModelSelector ─────────────────────────────────────────────────────────────

export function ModelSelector({ session, modelType }: { session: Session | null; modelType: "large" | "small" }) {
  const config = useStore($config);
  const recentLarge = useStore($recentLargeModels);
  const recentSmall = useStore($recentSmallModels);

  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  // When non-null, show API key form for this provider
  const [keyFormProvider, setKeyFormProvider] = useState<{ id: string; name: string } | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  const btnRef = useRef<HTMLButtonElement>(null);
  const [dropdownPos, setDropdownPos] = useState<{ left: number; bottom: number }>({ left: 0, bottom: 0 });

  function updatePos() {
    if (!btnRef.current) return;
    const r = btnRef.current.getBoundingClientRect();
    const width = 520;
    const margin = 8;
    const left = Math.min(r.left, window.innerWidth - width - margin);
    setDropdownPos({ left: Math.max(margin, left), bottom: window.innerHeight - r.top + 8 });
  }

  const allModels = useMemo(() => buildModelList(config), [config]);
  const providerGroups = useMemo(() => buildProviderGroups(config), [config]);
  const defaultKey = useMemo(() => getDefaultModelKey(modelType, config), [modelType, config]);
  const recentKeys = modelType === "large" ? recentLarge : recentSmall;

  // Get current key from session record if available, else use global default
  let currentKey = defaultKey;
  if (session) {
    const p = modelType === "large" ? session.LargeModelProvider : session.SmallModelProvider;
    const m = modelType === "large" ? session.LargeModelID : session.SmallModelID;
    if (p && m) {
      currentKey = `${p}:::${m}`;
    }
  }

  const currentEntry = allModels.find(m => m.key === currentKey);
  const displayName = currentEntry?.name ?? currentKey.split(":::")[1] ?? "No model";

  // Get current reasoning effort (default to "medium" if not set)
  const currentProvider = currentEntry?.providerID ?? "";
  const currentModelID = currentEntry?.modelID ?? "";
  const isCLIClaudeModelFlag = isCLIClaudeModel(currentProvider, currentModelID);
  const isZAIReasoningFlag = isZAIReasoningModel(currentProvider, currentModelID);
  const effortLevels: readonly string[] = isZAIReasoningFlag ? EFFORT_LEVELS_ZAI : EFFORT_LEVELS;
  // Default: Claude CLI keeps the legacy "medium"; z.ai GLM-5.x defaults
  // to "high" (Max is opt-in for heavy work — same wording z.ai uses).
  let currentEffort = isZAIReasoningFlag ? "high" : "medium";
  if (session) {
    const effort = modelType === "large" ? session.LargeModelReasoningEffort : session.SmallModelReasoningEffort;
    if (effort) currentEffort = effort;
  }
  const showEffortPicker = isCLIClaudeModelFlag || isZAIReasoningFlag;

  function cycleEffort(direction: 1 | -1) {
    if (!session || !showEffortPicker) return;
    let idx = effortLevels.indexOf(currentEffort);
    if (idx === -1) {
      // Picked a value the current selector doesn't own (e.g. switched
      // model from Claude → GLM and "medium" isn't in the z.ai set).
      // Reset to the first level so the chevron behaves predictably.
      idx = 0;
    }
    const newIdx = (idx + direction + effortLevels.length) % effortLevels.length;
    const newEffort = effortLevels[newIdx];
    setSessionReasoningEffort(
      session.ID,
      modelType === "large" ? newEffort : null,
      modelType === "small" ? newEffort : null,
    );
  }

  const recentModels = useMemo(() => {
    return recentKeys
      .map(k => allModels.find(m => m.key === k))
      .filter((m): m is ModelItem => !!m);
  }, [recentKeys, allModels]);

  const q = search.toLowerCase();

  const searchResults = useMemo(() => {
    if (!q) return [];
    return allModels.filter(m =>
      m.name.toLowerCase().includes(q) ||
      m.providerID.toLowerCase().includes(q) ||
      m.providerName.toLowerCase().includes(q) ||
      m.modelID.toLowerCase().includes(q)
    );
  }, [allModels, q]);

  useEffect(() => {
    if (!open) return;
    updatePos();
    window.addEventListener("resize", updatePos);
    window.addEventListener("scroll", updatePos, true);
    function handler(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
        setKeyFormProvider(null);
      }
    }
    document.addEventListener("mousedown", handler);
    return () => {
      document.removeEventListener("mousedown", handler);
      window.removeEventListener("resize", updatePos);
      window.removeEventListener("scroll", updatePos, true);
    };
  }, [open]);

  const Icon = modelType === "large" ? BrainCircuit : Zap;
  const title = modelType === "large" ? "Large (strong) model" : "Small (fast) model";

  function onSelect(m: ModelItem) {
    if (!m.enabled && m.providerType !== "cli") {
      // Show inline API key form for API-based providers
      setKeyFormProvider({ id: m.providerID, name: m.providerName });
      setSearch("");
      return;
    }
    if (!m.enabled) return; // CLI providers can't be selected without being enabled
    if (session) {
      const currentLargeKey = session.LargeModelProvider ? `${session.LargeModelProvider}:::${session.LargeModelID}` : getDefaultModelKey("large", config);
      const currentSmallKey = session.SmallModelProvider ? `${session.SmallModelProvider}:::${session.SmallModelID}` : getDefaultModelKey("small", config);

      const largeKey = modelType === "large" ? m.key : currentLargeKey;
      const smallKey = modelType === "small" ? m.key : currentSmallKey;

      setSessionModels(session.ID, largeKey, smallKey);
      trackModelUsage(modelType, m.key);
      setOpen(false);
    }
  }

  if (!session || allModels.length === 0) {
    return (
      <span className="flex items-center gap-1.5 text-xs text-text-subtle bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5" title={title}>
        <Icon size={12} />
        {displayName}
      </span>
    );
  }

  const recentKeySet = new Set(recentKeys);

  return (
    <div ref={ref} className="relative">
      <button
        ref={btnRef}
        onClick={() => { setOpen(o => !o); setSearch(""); setKeyFormProvider(null); }}
        className="flex items-center gap-1.5 text-xs text-text bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 hover:border-accent/50 hover:bg-base-subtle transition-colors"
        title={title}
        data-test-id={modelType === "large" ? "model-selector-large" : "model-selector-small"}
      >
        <Icon size={12} className="shrink-0" />
        <span className="font-medium truncate max-w-[180px]">{displayName}</span>
        {showEffortPicker && (
          <div
            className="flex items-center gap-0.5 shrink-0 ml-1"
            onClick={e => e.stopPropagation()}
            data-test-id={`reasoning-effort-${modelType}`}
          >
            <button
              onClick={() => cycleEffort(-1)}
              className="p-0.5 rounded hover:bg-base-subtle text-text-subtle hover:text-text transition-colors"
              title={`Reasoning effort: ${currentEffort} (click to decrease)`}
              data-test-id={`reasoning-effort-${modelType}-decrease`}
            >
              <ChevronLeft size={12} strokeWidth={2.5} />
            </button>
            <span
              className="px-1 py-0.5 rounded bg-base-subtle text-text font-mono text-[10px] min-w-[16px] text-center"
              title={`Reasoning effort: ${currentEffort}`}
              data-test-id={`reasoning-effort-${modelType}-label`}
            >
              {EFFORT_LABELS[currentEffort] ?? "?"}
            </span>
            <button
              onClick={() => cycleEffort(1)}
              className="p-0.5 rounded hover:bg-base-subtle text-text-subtle hover:text-text transition-colors"
              title={`Reasoning effort: ${currentEffort} (click to increase)`}
              data-test-id={`reasoning-effort-${modelType}-increase`}
            >
              <ChevronRight size={12} strokeWidth={2.5} />
            </button>
          </div>
        )}
        <span className="text-text-subtle ml-auto">{open ? "▴" : "▾"}</span>
      </button>
      {open && (
        <div
          data-test-id="model-dropdown"
          style={{ position: "fixed", left: dropdownPos.left, bottom: dropdownPos.bottom, width: 520, zIndex: 9999 }}
          className="bg-canvas border border-surface rounded-xl shadow-xl overflow-hidden"
        >
          {keyFormProvider ? (
            <APIKeyForm
              providerID={keyFormProvider.id}
              providerName={keyFormProvider.name}
              onDone={() => { setKeyFormProvider(null); setOpen(false); }}
              onCancel={() => setKeyFormProvider(null)}
            />
          ) : (
            <>
            <div className="max-h-[480px] overflow-y-auto">
              {q ? (
                searchResults.length === 0 ? (
                  <p className="text-text-subtle text-sm text-center py-4">No models found</p>
                ) : (
                  searchResults.map(m => (
                    <ModelRow key={m.key} model={m} isSelected={m.key === currentKey} onSelect={onSelect} />
                  ))
                )
              ) : (
                <>
                  {recentModels.length > 0 && (
                    <div className="py-1">
                      <div className="px-3 py-1.5 text-[10px] font-bold text-text-muted uppercase tracking-wider">Recent</div>
                      {recentModels.map(m => (
                        <div key={m.key} className="flex items-center group/row">
                          <div className="flex-1 min-w-0">
                            <ModelRow model={m} isSelected={m.key === currentKey} onSelect={onSelect} />
                          </div>
                          <button
                            onClick={e => { e.stopPropagation(); removeRecentModel(modelType, m.key); }}
                            title="Remove from recent"
                            className="shrink-0 px-2 py-2 text-text-subtle hover:text-red opacity-0 group-hover/row:opacity-100 transition-opacity text-xs"
                          >
                            ✕
                          </button>
                        </div>
                      ))}
                      <div className="h-px bg-surface/40 my-1" />
                    </div>
                  )}
                  {providerGroups.map(group => {
                    const groupModels = group.models.filter(m => !recentKeySet.has(m.key));
                    if (groupModels.length === 0) return null;
                    return (
                      <div key={group.id} className="py-1">
                        <div className="px-3 py-1.5 flex items-center gap-2">
                          <span className="text-[10px] font-bold text-text-muted uppercase tracking-wider">{group.name}</span>
                          {group.type !== "cli" && (
                            group.enabled ? (
                              <div className="flex items-center gap-1 ml-auto">
                                <button
                                  onClick={e => { e.stopPropagation(); setKeyFormProvider({ id: group.id, name: group.name }); }}
                                  title="Edit API key"
                                  className="text-[10px] text-text-subtle hover:text-accent transition-colors px-1"
                                >
                                  Edit key
                                </button>
                                <button
                                  onClick={e => { e.stopPropagation(); removeProviderKey(group.id); }}
                                  title="Remove API key"
                                  className="text-[10px] text-text-subtle hover:text-red transition-colors px-1"
                                >
                                  Remove key
                                </button>
                              </div>
                            ) : (
                              <button
                                onClick={e => { e.stopPropagation(); setKeyFormProvider({ id: group.id, name: group.name }); }}
                                className="text-[9px] text-accent border border-accent/40 rounded px-1.5 py-0.5 hover:bg-accent/10 transition-colors"
                              >
                                + Add API key
                              </button>
                            )
                          )}
                        </div>
                        {groupModels.map(m => (
                          <ModelRow key={m.key} model={m} isSelected={m.key === currentKey} onSelect={onSelect} />
                        ))}
                      </div>
                    );
                  })}
                </>
              )}
            </div>
            <div className="p-2.5 border-t border-surface/40">
              <input
                autoFocus
                value={search}
                onChange={e => setSearch(e.target.value)}
                placeholder="Search models…"
                className="w-full bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 text-sm text-text outline-none focus:border-accent transition-colors placeholder:text-text-subtle"
              />
            </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}
