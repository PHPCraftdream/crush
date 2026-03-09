import { useState, useRef, useEffect, useMemo } from "react";
import { useStore } from "@nanostores/react";
import { $sessions, $activeSessionID, $config, $busySessions, $sessionLargeModel, $sessionSmallModel, setSessionLargeModel, setSessionSmallModel } from "../store";
import { Settings } from "./Settings";
import { ws } from "../ws";
import type { ConfigPayload } from "../types";

function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}

// Builds a flat list of all models from all providers, keyed as "providerID:::modelID"
function buildModelList(config: ConfigPayload | null) {
  const result: Array<{ key: string; providerID: string; modelID: string; name: string }> = [];
  const seen = new Set<string>();
  for (const [providerID, p] of Object.entries(config?.providers ?? {})) {
    for (const m of (p.models ?? [])) {
      const key = `${providerID}:::${m.id}`;
      if (!seen.has(key)) {
        seen.add(key);
        result.push({ key, providerID, modelID: m.id, name: m.name || m.id });
      }
    }
  }
  // Fallback: if no providers, expose config.models entries
  if (result.length === 0) {
    for (const [, m] of Object.entries(config?.models ?? {})) {
      const key = `${m.Provider}:::${m.Model}`;
      if (!seen.has(key)) {
        seen.add(key);
        result.push({ key, providerID: m.Provider, modelID: m.Model, name: m.Model });
      }
    }
  }
  return result;
}

// Default key from config.models for "large" or "small" role
function defaultKeyForRole(role: "large" | "small", config: ConfigPayload | null): string {
  const entry = config?.models?.[role];
  if (entry) return `${entry.Provider}:::${entry.Model}`;
  return "";
}

function ModelSelector({ sessionID, modelType }: { sessionID: string | null; modelType: "large" | "small" }) {
  const config = useStore($config);
  const sessionLargeModels = useStore($sessionLargeModel);
  const sessionSmallModels = useStore($sessionSmallModel);
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const ref = useRef<HTMLDivElement>(null);

  const allModels = useMemo(() => buildModelList(config), [config]);
  const defaultKey = useMemo(() => defaultKeyForRole(modelType, config), [modelType, config]);

  const storedKey = sessionID
    ? (modelType === "large" ? sessionLargeModels[sessionID] : sessionSmallModels[sessionID])
    : undefined;
  const currentKey = storedKey ?? defaultKey;

  const currentEntry = allModels.find(m => m.key === currentKey);
  const displayName = currentEntry?.name ?? currentKey.split(":::")[1] ?? "No model";

  const q = search.toLowerCase();
  const filtered = allModels.filter(m =>
    m.name.toLowerCase().includes(q) ||
    m.providerID.toLowerCase().includes(q) ||
    m.modelID.toLowerCase().includes(q)
  );

  useEffect(() => {
    function handler(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    if (open) document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  const setModel = modelType === "large" ? setSessionLargeModel : setSessionSmallModel;
  const icon = modelType === "large" ? "🧠" : "⚡";
  const title = modelType === "large" ? "Large (strong) model" : "Small (fast) model";

  if (!sessionID || allModels.length === 0) {
    return (
      <span className="text-xs text-text-subtle bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5" title={title}>
        {icon} {displayName}
      </span>
    );
  }

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => { setOpen(o => !o); setSearch(""); }}
        className="flex items-center gap-1.5 text-xs text-text bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 hover:border-accent/50 hover:bg-base-subtle transition-colors"
        title={title}
      >
        <span>{icon}</span>
        <span className="font-medium truncate max-w-[220px]">{displayName}</span>
        <span className="text-text-subtle">{open ? "▴" : "▾"}</span>
      </button>
      {open && (
        <div className="absolute right-0 top-full mt-2 w-[480px] bg-white border border-surface rounded-xl shadow-lg z-50 overflow-hidden">
          <div className="p-2.5 border-b border-surface">
            <input
              autoFocus
              value={search}
              onChange={e => setSearch(e.target.value)}
              placeholder="Search models…"
              className="w-full bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 text-sm text-text outline-none focus:border-accent transition-colors placeholder:text-text-subtle"
            />
          </div>
          <div className="max-h-56 overflow-y-auto">
            {filtered.length === 0 && (
              <p className="text-text-subtle text-sm text-center py-4">No models found</p>
            )}
            {filtered.map(m => (
              <button
                key={m.key}
                onClick={() => { setModel(sessionID, m.key); setOpen(false); }}
                className={`w-full text-left px-3 py-2.5 hover:bg-base-overlay transition-colors border-b border-surface/50 last:border-0 ${
                  m.key === currentKey ? "bg-accent/5" : ""
                }`}
              >
                <div className={`text-sm font-medium truncate ${m.key === currentKey ? "text-accent" : "text-text"}`}>
                  {m.name}
                </div>
                <div className="text-xs text-text-subtle mt-0.5">{m.providerID}</div>
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
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
    return <h1 className="text-base text-text-subtle">No session selected</h1>;
  }

  if (editing) {
    return (
      <input
        ref={inputRef}
        value={value}
        onChange={e => setValue(e.target.value)}
        onBlur={commit}
        onKeyDown={onKey}
        className="text-base font-semibold text-text bg-base-overlay border border-accent/50 rounded-lg px-2 py-0.5 outline-none w-full max-w-xs"
      />
    );
  }

  return (
    <button
      onClick={startEdit}
      title="Click to rename"
      className="group flex items-center gap-1.5 text-left min-w-0"
    >
      <h1 className="text-base font-semibold text-text truncate">
        {session.Title || "Untitled session"}
      </h1>
      <span className="text-text-subtle opacity-0 group-hover:opacity-100 transition-opacity text-xs shrink-0">✎</span>
    </button>
  );
}

export function Header() {
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const [settingsOpen, setSettingsOpen] = useState(false);

  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;
  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const totalTokens = activeSession ? activeSession.PromptTokens + activeSession.CompletionTokens : 0;
  const isSummarized = !!activeSession?.SummaryMessageID;

  return (
    <>
      <header className="flex items-center gap-4 px-6 py-3.5 border-b border-surface bg-white shrink-0">
        <div className="flex-1 min-w-0">
          <SessionTitle session={activeSession} />
        </div>

        <div className="flex items-center gap-2 shrink-0">
          <ModelSelector sessionID={activeSessionID} modelType="large" />
          <ModelSelector sessionID={activeSessionID} modelType="small" />

          {activeSession && totalTokens > 0 && (
            <span
              className="text-xs text-text-subtle bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 flex items-center gap-1"
              title="Total tokens used"
            >
              {formatTokens(totalTokens)} tok
              {isSummarized && (
                <span className="text-accent" title="Session has been summarized">∑</span>
              )}
            </span>
          )}

          {isBusy && (
            <div className="flex items-center gap-1 animate-pulse-dots" title="Agent is working…">
              <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block" />
              <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block" />
              <span className="w-1.5 h-1.5 rounded-full bg-accent inline-block" />
            </div>
          )}

          <button
            onClick={() => setSettingsOpen(o => !o)}
            title="Settings"
            className="w-8 h-8 flex items-center justify-center rounded-lg border border-surface bg-base-overlay text-text-muted hover:text-text hover:border-accent/50 transition-colors"
          >
            ⚙
          </button>
        </div>
      </header>

      {settingsOpen && <Settings onClose={() => setSettingsOpen(false)} />}
    </>
  );
}
