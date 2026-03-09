import { useState, useRef, useEffect, useMemo } from "react";
import { useStore } from "@nanostores/react";
import {
  $sessions,
  $activeSessionID,
  $config,
  $busySessions,
  $recentLargeModels,
  $recentSmallModels,
  $yolo,
  trackModelUsage,
  removeRecentModel,
  getDefaultModelKey,
  setSessionModels,
  setYolo,
  setProviderKey,
  removeProviderKey,
} from "../store";
import { ws } from "../ws";
import type { ConfigPayload, Session } from "../types";

function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}

interface ModelItem {
  key: string;
  providerID: string;
  providerName: string;
  providerType: string;
  modelID: string;
  name: string;
  contextWindow: number;
  enabled: boolean; // provider has an API key configured
}

interface ProviderGroup {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  models: ModelItem[];
}

// Builds a list of provider groups, each with their models
function buildProviderGroups(config: ConfigPayload | null): ProviderGroup[] {
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
function buildModelList(config: ConfigPayload | null): ModelItem[] {
  return buildProviderGroups(config).flatMap(g => g.models);
}

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
    <div className="p-3 border-b border-surface bg-base-overlay/50" onClick={e => e.stopPropagation()}>
      <div className="text-xs font-semibold text-text mb-1.5">{providerName} — Enter API key</div>
      <div className="flex gap-2">
        <input
          ref={inputRef}
          type="password"
          value={key}
          onChange={e => setKey(e.target.value)}
          onKeyDown={onKey}
          placeholder="sk-…"
          className="flex-1 bg-white border border-surface rounded-lg px-2.5 py-1.5 text-sm text-text outline-none focus:border-accent transition-colors placeholder:text-text-subtle font-mono"
        />
        <button
          onClick={save}
          disabled={!key.trim() || saving}
          className="px-3 py-1.5 bg-accent text-white text-xs font-medium rounded-lg disabled:opacity-40 hover:opacity-90 transition-opacity"
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

function ModelSelector({ session, modelType }: { session: Session | null; modelType: "large" | "small" }) {
  const config = useStore($config);
  const recentLarge = useStore($recentLargeModels);
  const recentSmall = useStore($recentSmallModels);

  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  // When non-null, show API key form for this provider
  const [keyFormProvider, setKeyFormProvider] = useState<{ id: string; name: string } | null>(null);
  const ref = useRef<HTMLDivElement>(null);

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
    function handler(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
        setKeyFormProvider(null);
      }
    }
    if (open) document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  const icon = modelType === "large" ? "🧠" : "⚡";
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
      <span className="text-xs text-text-subtle bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5" title={title}>
        {icon} {displayName}
      </span>
    );
  }

  const recentKeySet = new Set(recentKeys);

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => { setOpen(o => !o); setSearch(""); setKeyFormProvider(null); }}
        className="flex items-center gap-1.5 text-xs text-text bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 hover:border-accent/50 hover:bg-base-subtle transition-colors"
        title={title}
      >
        <span>{icon}</span>
        <span className="font-medium truncate max-w-[220px]">{displayName}</span>
        <span className="text-text-subtle">{open ? "▴" : "▾"}</span>
      </button>
      {open && (
        <div className="absolute right-0 top-full mt-2 w-[520px] bg-white border border-surface rounded-xl shadow-lg z-50 overflow-hidden">
          {keyFormProvider ? (
            <APIKeyForm
              providerID={keyFormProvider.id}
              providerName={keyFormProvider.name}
              onDone={() => { setKeyFormProvider(null); setOpen(false); }}
              onCancel={() => setKeyFormProvider(null)}
            />
          ) : (
            <div className="p-2.5 border-b border-surface">
              <input
                autoFocus
                value={search}
                onChange={e => setSearch(e.target.value)}
                placeholder="Search models…"
                className="w-full bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 text-sm text-text outline-none focus:border-accent transition-colors placeholder:text-text-subtle"
              />
            </div>
          )}
          {!keyFormProvider && (
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
                            className="shrink-0 px-2 py-2 text-text-subtle hover:text-red opacity-0 group-hover/row:opacity-100 transition-opacity"
                          >
                            ✕
                          </button>
                        </div>
                      ))}
                      <div className="h-px bg-surface my-1" />
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
          )}
        </div>
      )}
    </div>
  );
}

function ModelRow({ model, isSelected, onSelect }: { model: ModelItem, isSelected: boolean, onSelect: (m: ModelItem) => void }) {
  const disabled = !model.enabled;
  const isCLI = model.providerType === "cli";
  const canAddKey = disabled && !isCLI;
  return (
    <button
      onClick={() => onSelect(model)}
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

function SessionTitle({ session }: { session: { ID: string; Title: string } | null }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  function startEdit() {
    if (!session) return;
    setValue(session.Title || "Untitled session");
    setEditing(true);
  }

  useEffect(() => {
    if (editing) inputRef.current?.select();
  }, [editing]);

  function commit() {
    if (!session) return;
    const trimmed = value.trim();
    if (trimmed && trimmed !== session.Title) {
      ws.send("rename_session", { sessionID: session.ID, title: trimmed });
    }
    setEditing(false);
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Enter") commit();
    if (e.key === "Escape") setEditing(false);
  }

  if (!session) {
    return <h1 className="text-lg text-text-subtle">No session selected</h1>;
  }

  if (editing) {
    return (
      <input
        ref={inputRef}
        value={value}
        onChange={e => setValue(e.target.value)}
        onBlur={commit}
        onKeyDown={onKey}
        className="text-lg font-bold text-text bg-base-overlay border border-accent/50 rounded-xl px-3 py-1 outline-none w-full max-w-md"
      />
    );
  }

  return (
    <button
      onClick={startEdit}
      title="Click to rename"
      className="group flex items-center gap-2 text-left min-w-0"
    >
      <h1 className="text-lg font-bold text-text truncate">
        {session.Title || "Untitled session"}
      </h1>
      <span className="text-text-subtle opacity-0 group-hover:opacity-100 transition-opacity text-sm shrink-0">✎</span>
    </button>
  );
}

export function Header() {
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const yolo = useStore($yolo);
  const config = useStore($config);

  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;
  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const totalTokens = activeSession ? activeSession.PromptTokens + activeSession.CompletionTokens : 0;
  const isSummarized = !!activeSession?.SummaryMessageID;


  // Determine context window from the effective large model of the active session.
  // Use the same key-resolution logic as ModelSelector so the % always matches
  // the model displayed in the header.
  const allModels = useMemo(() => buildModelList(config), [config]);
  const effectiveLargeKey = useMemo(() => {
    if (!activeSession) return getDefaultModelKey("large", config);
    const p = activeSession.LargeModelProvider;
    const m = activeSession.LargeModelID;
    if (p && m) return `${p}:::${m}`;
    return getDefaultModelKey("large", config);
  }, [activeSession, config]);
  const contextWindow = useMemo(() => {
    if (!effectiveLargeKey) return 0;
    return allModels.find(x => x.key === effectiveLargeKey)?.contextWindow ?? 0;
  }, [effectiveLargeKey, allModels]);

  const contextPct = contextWindow > 0 ? Math.min(100, Math.round((totalTokens / contextWindow) * 100)) : null;

  // Color ramp: green → yellow → red
  function pctColor(pct: number): string {
    if (pct >= 85) return "text-red";
    if (pct >= 60) return "text-yellow";
    return "text-green";
  }

  return (
    <header className="flex items-center gap-6 px-8 py-8 border-b border-surface bg-white shrink-0">
      <div className="flex-1 min-w-0">
        <SessionTitle session={activeSession} />
      </div>

      <div className="flex items-center gap-3 shrink-0">
        <ModelSelector session={activeSession} modelType="large" />
        <ModelSelector session={activeSession} modelType="small" />

        {activeSession && totalTokens > 0 && (
          <span
            className="text-sm font-medium text-text-subtle bg-base-overlay border border-surface rounded-xl px-3.5 py-2 flex items-center gap-1.5"
            title={contextWindow > 0 ? `${totalTokens.toLocaleString()} / ${contextWindow.toLocaleString()} tokens` : `${totalTokens.toLocaleString()} tokens used`}
          >
            {formatTokens(totalTokens)}
            {contextPct !== null && (
              <span className={`font-semibold ${pctColor(contextPct)}`}>{contextPct}%</span>
            )}
            {isSummarized && (
              <span className="text-accent" title="Session has been summarized">∑</span>
            )}
          </span>
        )}

        <button
          onClick={() => setYolo(!yolo)}
          title={yolo ? "Yolo mode ON — all tool calls auto-approved. Click to disable." : "Yolo mode OFF — tool calls require approval. Click to enable."}
          className={`flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors ${
            yolo
              ? "bg-yellow/10 border-yellow/40 text-yellow hover:bg-yellow/20"
              : "bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
          }`}
        >
          <span>{yolo ? "⚡" : "🔒"}</span>
          <span>Yolo</span>
        </button>

        {isBusy && (
          <div className="flex items-center gap-1.5 animate-pulse-dots px-2" title="Agent is working…">
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
          </div>
        )}
      </div>
    </header>
  );
}
