import { useState, useRef, useEffect, useMemo, useCallback } from "react";
import { useStore } from "@nanostores/react";
import { CheckCheck, ScrollText, Plug, Sun, Moon, Code2, Settings, ServerCog, FileText, Folder } from "lucide-react";
import { LogsModal } from "./LogsModal";
import {
  $sessions,
  $activeSessionID,
  $config,
  $busySessions,
  setTheme,
} from "../store";
import { ws } from "../ws";
import { MCPSettings } from "./MCPSettings";
import { LSPSettings } from "./LSPSettings";
import { SettingsModal } from "./SettingsModal";
import { ProvidersModal } from "./ProvidersModal";
import { buildModelList } from "./ModelSelector";
import { getDefaultModelKey } from "../store";

// ── System Prompt Modal ───────────────────────────────────────────────────────

function SystemPromptModal({ sessionID, onClose }: { sessionID: string; onClose: () => void }) {
  const [original, setOriginal] = useState<string>("");
  const [draft, setDraft] = useState<string>("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  const dirty = draft !== original;

  useEffect(() => {
    const unsub = ws.on("system_prompt", (msg) => {
      const p = msg.payload as { content?: string } | undefined;
      const c = p?.content ?? "";
      setOriginal(c);
      setDraft(c);
      setLoading(false);
      unsub();
    });
    ws.send("get_system_prompt", { sessionID });

    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [sessionID, onClose]);

  function save() {
    setSaving(true);
    ws.send("set_system_prompt", { sessionID, content: draft });
    setOriginal(draft);
    setSaving(false);
  }

  function reset() {
    setDraft(original);
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-xl flex flex-col w-full max-w-3xl mx-4 max-h-[85vh]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-6 py-4 border-b border-surface shrink-0">
          <h2 className="text-base font-semibold text-text">System Prompt</h2>
          <button
            onClick={onClose}
            className="text-text-subtle hover:text-text transition-colors text-xl leading-none"
          >
            ×
          </button>
        </div>
        <div className="flex-1 overflow-hidden p-4">
          {loading ? (
            <p className="text-text-subtle text-sm p-2">Loading…</p>
          ) : (
            <textarea
              className="w-full h-full min-h-[400px] text-xs font-mono text-text bg-base-overlay border border-surface rounded-xl p-3 resize-none outline-none focus:border-accent/50 leading-relaxed"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              spellCheck={false}
            />
          )}
        </div>
        <div className="flex items-center justify-end gap-3 px-6 py-4 border-t border-surface shrink-0">
          {dirty && (
            <button
              onClick={reset}
              className="px-4 py-2 text-sm text-text-subtle hover:text-text transition-colors rounded-xl hover:bg-base-overlay"
            >
              Reset
            </button>
          )}
          <button
            onClick={save}
            disabled={!dirty || saving}
            className="px-4 py-2 text-sm font-medium bg-accent-fill text-white/90 rounded-xl hover:opacity-90 transition-opacity disabled:opacity-40 disabled:cursor-not-allowed"
          >
            Save
          </button>
        </div>
      </div>
    </div>
  );
}

function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}

function SessionTitle({ session, cwd }: { session: { ID: string; Title: string } | null; cwd?: string }) {
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
    <div className="flex flex-col gap-1 min-w-0">
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
      {cwd && (
        <div className="flex items-center gap-1.5 text-xs text-text-subtle truncate" title={cwd}>
          <Folder size={12} className="shrink-0" />
          <span className="truncate">{cwd}</span>
        </div>
      )}
    </div>
  );
}

export function Header() {
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const config = useStore($config);
  const [showSystemPrompt, setShowSystemPrompt] = useState(false);
  const closeSystemPrompt = useCallback(() => setShowSystemPrompt(false), []);
  const [showMCPSettings, setShowMCPSettings] = useState(false);
  const closeMCPSettings = useCallback(() => setShowMCPSettings(false), []);
  const [showLSPSettings, setShowLSPSettings] = useState(false);
  const closeLSPSettings = useCallback(() => setShowLSPSettings(false), []);
  const [showSettings, setShowSettings] = useState(false);
  const closeSettings = useCallback(() => setShowSettings(false), []);
  const [showProviders, setShowProviders] = useState(false);
  const closeProviders = useCallback(() => setShowProviders(false), []);
  const [showLogs, setShowLogs] = useState(false);
  const closeLogs = useCallback(() => setShowLogs(false), []);

  const isDark = config?.theme === "dark";
  function toggleTheme() {
    setTheme(isDark ? "light" : "dark");
  }
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

  // Display name of the currently active large model (for progress indicator)
  const activeLargeModelName = useMemo(() => {
    if (!effectiveLargeKey) return null;
    return allModels.find(x => x.key === effectiveLargeKey)?.name ?? null;
  }, [effectiveLargeKey, allModels]);

  const contextPct = contextWindow > 0 ? Math.min(100, Math.round((totalTokens / contextWindow) * 100)) : null;

  // Color ramp: green → yellow → red
  function pctColor(pct: number): string {
    if (pct >= 85) return "text-red";
    if (pct >= 60) return "text-yellow";
    return "text-green";
  }

  return (
    <>
    <header className="flex items-center gap-6 px-8 py-6 border-b border-surface bg-canvas shrink-0">
      <div className="flex-1 min-w-0">
        <SessionTitle session={activeSession} cwd={config?.cwd} />
      </div>

      <div className="flex items-center gap-3 shrink-0">
        {activeSession && totalTokens > 0 && (
          <span
            className="text-sm font-medium text-text-subtle bg-base-overlay border border-surface rounded-xl px-3.5 py-2 flex items-center gap-1.5"
            title={`${totalTokens.toLocaleString()} tokens total across all requests in this session (includes system prompt + tool definitions sent each turn)${contextWindow > 0 ? ` · context window: ${contextWindow.toLocaleString()}` : ""}`}
          >
            {formatTokens(totalTokens)}
            {contextPct !== null && (
              <span className={`font-semibold ${pctColor(contextPct)}`}>{contextPct}%</span>
            )}
            {isSummarized && (
              <span title="Session has been summarized"><CheckCheck size={13} className="text-accent" /></span>
            )}
          </span>
        )}

        <button
          onClick={() => setShowSystemPrompt(true)}
          disabled={!activeSessionID}
          title="View / edit system prompt"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text disabled:opacity-40 disabled:cursor-not-allowed"
        >
          <ScrollText size={13} />
          <span>Prompt</span>
        </button>

        <button
          onClick={() => setShowMCPSettings(true)}
          title="MCP server settings"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <Plug size={13} />
          <span>MCP</span>
        </button>

        <button
          onClick={() => setShowLSPSettings(true)}
          title="LSP server settings"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <Code2 size={13} />
          <span>LSP</span>
        </button>

        <button
          onClick={() => setShowProviders(true)}
          title="Custom providers"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <ServerCog size={13} />
          <span>Providers</span>
        </button>

        <button
          onClick={() => setShowSettings(true)}
          title="Settings"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <Settings size={13} />
          <span>Settings</span>
        </button>

        <button
          onClick={() => setShowLogs(true)}
          title="View logs"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <FileText size={13} />
          <span>Logs</span>
        </button>

        <button
          onClick={toggleTheme}
          title={isDark ? "Switch to light theme" : "Switch to dark theme"}
          className="flex items-center justify-center w-8 h-8 rounded-lg border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          {isDark ? <Sun size={14} /> : <Moon size={14} />}
        </button>

        {isBusy && (
          <div className="flex items-center gap-2 animate-pulse-dots px-2" title={activeLargeModelName ? `Running ${activeLargeModelName}…` : "Agent is working…"}>
            {activeLargeModelName && (
              <span className="text-xs text-text-subtle font-medium">{activeLargeModelName}</span>
            )}
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
          </div>
        )}
      </div>
    </header>
    {showSystemPrompt && activeSessionID && <SystemPromptModal sessionID={activeSessionID} onClose={closeSystemPrompt} />}
    {showMCPSettings && <MCPSettings onClose={closeMCPSettings} />}
    {showLSPSettings && <LSPSettings onClose={closeLSPSettings} />}
    {showSettings && <SettingsModal onClose={closeSettings} />}
    {showProviders && <ProvidersModal onClose={closeProviders} />}
    {showLogs && <LogsModal onClose={closeLogs} />}}
</>
  );
}
