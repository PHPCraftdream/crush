import { useState, useEffect, useRef } from "react";
import { useStore } from "@nanostores/react";
import { $config, addContextPath, removeContextPath, addSkillsPath, removeSkillsPath, initializeProject, setActiveSession } from "../store";
import { ws } from "../ws";
import { X, Plus, Trash2, RefreshCw, FolderOpen, Loader2 } from "lucide-react";
import type { SkillsSnapshot } from "../types";

// ── Toggle switch ─────────────────────────────────────────────────────────────

function Toggle({ checked, onChange, label }: { checked: boolean; onChange: (v: boolean) => void; label: string }) {
  return (
    <label className="flex items-center gap-3 cursor-pointer select-none">
      <button
        role="switch"
        aria-checked={checked}
        onClick={() => onChange(!checked)}
        className={`relative w-10 h-6 rounded-full transition-colors focus:outline-none ${checked ? "bg-accent" : "bg-surface"}`}
      >
        <span className={`absolute top-1 left-1 w-4 h-4 rounded-full bg-white shadow transition-transform ${checked ? "translate-x-4" : "translate-x-0"}`} />
      </button>
      <span className="text-sm text-text">{label}</span>
    </label>
  );
}

// ── Path list (reused for context_paths and skills_paths) ─────────────────────

function PathList({
  paths,
  onAdd,
  onRemove,
  placeholder,
}: {
  paths: string[];
  onAdd: (p: string) => void;
  onRemove: (p: string) => void;
  placeholder: string;
}) {
  const [draft, setDraft] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  function add() {
    const v = draft.trim();
    if (!v) return;
    onAdd(v);
    setDraft("");
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Enter") add();
  }

  return (
    <div className="space-y-2">
      {paths.length > 0 ? (
        <ul className="space-y-1">
          {paths.map((p) => (
            <li key={p} className="flex items-center gap-2 bg-base-overlay border border-surface rounded-lg px-3 py-1.5">
              <span className="flex-1 text-xs font-mono text-text truncate">{p}</span>
              <button
                onClick={() => onRemove(p)}
                className="shrink-0 text-text-subtle hover:text-red transition-colors"
                title="Remove"
              >
                <Trash2 size={13} />
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-xs text-text-muted italic">No paths configured</p>
      )}
      <div className="flex gap-2">
        <input
          ref={inputRef}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={onKey}
          placeholder={placeholder}
          className="flex-1 text-xs font-mono text-text bg-canvas border border-surface rounded-lg px-3 py-1.5 outline-none focus:border-accent/50 transition-colors placeholder:text-text-muted/50"
        />
        <button
          onClick={add}
          disabled={!draft.trim()}
          className="px-3 py-1.5 text-xs font-medium bg-accent-fill text-white/90 rounded-lg hover:opacity-90 disabled:opacity-40 flex items-center gap-1"
        >
          <Plus size={12} />
          Add
        </button>
      </div>
    </div>
  );
}

// ── Skills section ────────────────────────────────────────────────────────────

function SkillsSection({ skillsPaths: _skillsPaths }: { skillsPaths: string[] }) {
  const [snapshot, setSnapshot] = useState<SkillsSnapshot | null>(null);
  const [loading, setLoading] = useState(false);

  function refresh() {
    setLoading(true);
    const msgID = crypto.randomUUID();
    const unsub = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      unsub();
      setLoading(false);
      if (!msg.error) setSnapshot(msg.payload as SkillsSnapshot);
    });
    ws.send("get_skills", {}, msgID);
  }

  useEffect(() => { refresh(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-xs font-semibold text-text-subtle uppercase tracking-wider">Discovered Skills</span>
        <button
          onClick={refresh}
          disabled={loading}
          title="Refresh skills"
          className="text-text-subtle hover:text-accent transition-colors disabled:opacity-40"
        >
          <RefreshCw size={13} className={loading ? "animate-spin" : ""} />
        </button>
      </div>
      {loading ? (
        <p className="text-xs text-text-subtle">Scanning…</p>
      ) : snapshot && snapshot.skills.length > 0 ? (
        <ul className="space-y-1.5">
          {snapshot.skills.map((s) => (
            <li key={s.path} className="bg-base-overlay border border-surface rounded-lg px-3 py-2">
              <div className="text-xs font-semibold text-text">{s.name}</div>
              {s.description && <div className="text-[11px] text-text-subtle mt-0.5 line-clamp-2">{s.description}</div>}
              <div className="text-[10px] font-mono text-text-muted mt-1 truncate">{s.path}</div>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-xs text-text-muted italic">No skills found in configured paths</p>
      )}
    </div>
  );
}

// ── Main Settings Modal ───────────────────────────────────────────────────────

export function SettingsModal({ onClose }: { onClose: () => void }) {
  const config = useStore($config);
  const [initBusy, setInitBusy] = useState(false);
  const [initDone, setInitDone] = useState(false);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  function handleInitialize() {
    setInitBusy(true);
    const msgID = crypto.randomUUID();
    const unsub = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      unsub();
      setInitBusy(false);
      if (!msg.error) {
        setInitDone(true);
        // Navigate to the new session
        const payload = msg.payload as { sessionID?: string } | undefined;
        if (payload?.sessionID) {
          setActiveSession(payload.sessionID);
          onClose();
        }
      }
    });
    initializeProject(msgID);
  }

  const contextPaths = config?.contextPaths ?? [];
  const skillsPaths = config?.skillsPaths ?? [];

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
      onClick={onClose}
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[85vh] chat-font"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0">
          <div>
            <h2 className="text-base font-semibold text-text">Settings</h2>
            <p className="text-xs text-text-subtle mt-0.5">Context and skills configuration</p>
          </div>
          <button onClick={onClose} className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay">
            <X size={16} />
          </button>
        </div>

        {/* Body */}
        <div className="flex-1 overflow-y-auto divide-y divide-surface">

          {/* Initialize project */}
          <section className="px-5 py-4 space-y-3">
            <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-wider">Project Initialization</h3>
            <p className="text-xs text-text-subtle leading-relaxed">
              Analyze the codebase and create or update{" "}
              <span className="font-mono text-text">{config?.initializeAs ?? "AGENTS.md"}</span>{" "}
              to help the agent work more effectively.
            </p>
            <button
              onClick={handleInitialize}
              disabled={initBusy || initDone}
              className="flex items-center gap-2 px-4 py-2 text-sm font-medium bg-accent-fill text-white/90 rounded-xl hover:opacity-90 disabled:opacity-40 transition-all"
            >
              {initBusy ? (
                <><Loader2 size={14} className="animate-spin" /> Initializing…</>
              ) : initDone ? (
                <>Done!</>
              ) : (
                <><FolderOpen size={14} /> Initialize Project</>
              )}
            </button>
          </section>

          {/* Context paths */}
          <section className="px-5 py-4 space-y-3">
            <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-wider">Context Paths</h3>
            <p className="text-xs text-text-subtle leading-relaxed">
              Additional files the agent reads for project context (beyond defaults like AGENTS.md, CLAUDE.md).
            </p>
            <PathList
              paths={contextPaths}
              onAdd={addContextPath}
              onRemove={removeContextPath}
              placeholder="e.g. docs/architecture.md"
            />
          </section>

          {/* Skills paths */}
          <section className="px-5 py-4 space-y-3">
            <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-wider">Agent Skills Paths</h3>
            <p className="text-xs text-text-subtle leading-relaxed">
              Directories containing Agent Skills (folders with SKILL.md files).
            </p>
            <PathList
              paths={skillsPaths}
              onAdd={addSkillsPath}
              onRemove={removeSkillsPath}
              placeholder="e.g. ./project-skills"
            />
            <SkillsSection skillsPaths={skillsPaths} />
          </section>
        </div>

        {/* Footer */}
        <div className="px-5 py-3 border-t border-surface bg-base-overlay/50">
          <p className="text-xs text-text-subtle text-center">
            {config?.version || "Development build"}
          </p>
        </div>
      </div>
    </div>
  );
}
