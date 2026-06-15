import { useState, useEffect, useCallback, useMemo } from "react";
import { useStore } from "@nanostores/react";
import { Minimize2, Zap, ShieldOff, X, CheckCheck, ScrollText, Plug, Sun, Moon, Settings, ServerCog, FileText } from "lucide-react";
import {
  $sessions,
  $activeSessionID,
  $busySessions,
  $summarizeQueued,
  $yolo,
  $config,
  setYolo,
  summarizeSession,
  cancelQueuedSummarize,
  setTheme,
  getDefaultModelKey,
} from "../store";
import { ModelSelector, buildModelList } from "./ModelSelector";
import { LogsModal } from "./LogsModal";
import { MCPSettings } from "./MCPSettings";
import { SettingsModal } from "./SettingsModal";
import { ProvidersModal } from "./ProvidersModal";
import { ws } from "../ws";

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

function pctColor(pct: number): string {
  if (pct >= 85) return "text-red";
  if (pct >= 60) return "text-yellow";
  return "text-green";
}

export function ChatToolbar() {
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const summarizeQueued = useStore($summarizeQueued);
  const yolo = useStore($yolo);
  const config = useStore($config);

  // Modal state
  const [showSystemPrompt, setShowSystemPrompt] = useState(false);
  const closeSystemPrompt = useCallback(() => setShowSystemPrompt(false), []);
  const [showMCPSettings, setShowMCPSettings] = useState(false);
  const closeMCPSettings = useCallback(() => setShowMCPSettings(false), []);
  const [showSettings, setShowSettings] = useState(false);
  const closeSettings = useCallback(() => setShowSettings(false), []);
  const [showProviders, setShowProviders] = useState(false);
  const closeProviders = useCallback(() => setShowProviders(false), []);
  const [showLogs, setShowLogs] = useState(false);
  const closeLogs = useCallback(() => setShowLogs(false), []);

  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;
  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const isQueued = activeSessionID ? summarizeQueued.has(activeSessionID) : false;
  const hasMessages = (activeSession?.MessageCount ?? 0) > 0;

  const isDark = config?.theme === "dark";
  function toggleTheme() {
    setTheme(isDark ? "light" : "dark");
  }

  const totalTokens = activeSession ? activeSession.PromptTokens + activeSession.CompletionTokens : 0;
  const isSummarized = !!activeSession?.SummaryMessageID;

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

  const activeLargeModelName = useMemo(() => {
    if (!effectiveLargeKey) return null;
    return allModels.find(x => x.key === effectiveLargeKey)?.name ?? null;
  }, [effectiveLargeKey, allModels]);

  const contextPct = contextWindow > 0 ? Math.min(100, Math.round((totalTokens / contextWindow) * 100)) : null;

  if (!activeSessionID) return null;

  return (
    <>
      <div className="px-5 pt-3 pb-1 border-t border-surface bg-canvas shrink-0 flex items-center gap-2 flex-wrap">
        {/* LEFT cluster — migrated from Header */}
        {activeSession && totalTokens > 0 && (
          <span
            data-test-id="header-token-indicator"
            className="text-sm font-medium text-text-subtle bg-base-overlay border border-surface rounded-xl px-3.5 py-2 flex items-center gap-1.5"
            title={`${totalTokens.toLocaleString()} tokens total across all requests in this session (includes system prompt + tool definitions sent each turn)${contextWindow > 0 ? ` · context window: ${contextWindow.toLocaleString()}` : ""}`}
          >
            {formatTokens(totalTokens)}
            {contextPct !== null && (
              <span className={`font-semibold ${pctColor(contextPct)}`}>{contextPct}%</span>
            )}
            {isSummarized && (
              <span data-test-id="header-summarized-badge" title="Session has been summarized"><CheckCheck size={13} className="text-accent" /></span>
            )}
          </span>
        )}

        <button
          data-test-id="header-prompt-button"
          onClick={() => setShowSystemPrompt(true)}
          disabled={!activeSessionID}
          title="View / edit system prompt"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text disabled:opacity-40 disabled:cursor-not-allowed"
        >
          <ScrollText size={13} />
          <span>Prompt</span>
        </button>

        <button
          data-test-id="header-mcp-button"
          onClick={() => setShowMCPSettings(true)}
          title="MCP server settings"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <Plug size={13} />
          <span>MCP</span>
        </button>

        <button
          data-test-id="header-providers-button"
          onClick={() => setShowProviders(true)}
          title="Custom providers"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <ServerCog size={13} />
          <span>Providers</span>
        </button>

        <button
          data-test-id="header-settings-button"
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
          data-test-id="header-theme-toggle"
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

        <div className="mr-auto" />

        {/* RIGHT cluster — existing */}
        <ModelSelector session={activeSession} modelType="large" />
        <ModelSelector session={activeSession} modelType="small" />

        <div className="w-px h-4 bg-surface/50 mx-1 shrink-0" />

        {isQueued ? (
          <button
            onClick={() => cancelQueuedSummarize(activeSessionID)}
            title="Compact is queued — click to cancel"
            className="btn-toolbar text-accent border-accent/30 bg-accent/5 hover:bg-red/10 hover:text-red hover:border-red/30 flex items-center gap-1"
          >
            <Minimize2 size={13} />
            Compact queued
            <X size={11} className="opacity-60" />
          </button>
        ) : (
          <button
            onClick={() => summarizeSession(activeSessionID)}
            disabled={!hasMessages}
            title={isBusy ? "Compact will run after the current task finishes" : "Compact — compress conversation history to free up context window"}
            className="btn-toolbar"
          >
            <Minimize2 size={13} />
            Compact
          </button>
        )}

        <button
          onClick={() => activeSessionID && setYolo(activeSessionID, !yolo)}
          title={yolo ? "Yolo ON — all permissions auto-approved" : "Yolo OFF — tool calls require approval"}
          data-test-id="yolo-button"
          className={`btn-toolbar ${yolo ? "bg-yellow/10 border-yellow/30 text-yellow hover:bg-yellow/20" : ""}`}
        >
          {yolo ? <Zap size={13} /> : <ShieldOff size={13} />}
          Yolo
        </button>
      </div>

      {/* Modal hosts */}
      {showSystemPrompt && activeSessionID && <SystemPromptModal sessionID={activeSessionID} onClose={closeSystemPrompt} />}
      {showMCPSettings && <MCPSettings onClose={closeMCPSettings} />}
      {showSettings && <SettingsModal onClose={closeSettings} />}
      {showProviders && <ProvidersModal onClose={closeProviders} />}
      {showLogs && <LogsModal onClose={closeLogs} />}
    </>
  );
}
