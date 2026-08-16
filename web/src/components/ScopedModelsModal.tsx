import { useState, useEffect, useMemo, useCallback } from "react";
import { useStore } from "@nanostores/react";
import { X, Undo2 } from "lucide-react";
import { $config, clearSessionModelSlot } from "../store";
import { ws } from "../ws";
import { buildProviderGroups, buildModelList, type ModelItem } from "./ModelSelector";
import type { Session, WSMessage } from "../types";

// ── Wire types (mirror internal/server/protocol.go) ─────────────────────────

interface ModelOverrideWire {
  provider: string;
  model: string;
  reasoning_effort?: string;
}

interface ScopedModelSlotWire {
  global: ModelOverrideWire | null;
  workspace: ModelOverrideWire | null;
  effective: ModelOverrideWire | null;
  effectiveScope: "global" | "workspace" | "";
}

interface ScopedModelsWire {
  large: ScopedModelSlotWire;
  small: ScopedModelSlotWire;
  worker: ScopedModelSlotWire;
  reviewer: ScopedModelSlotWire;
  hasWorkspace: boolean;
}

type Slot = "large" | "small" | "worker" | "reviewer";
const SLOTS: { key: Slot; label: string }[] = [
  { key: "large", label: "Large (strong)" },
  { key: "small", label: "Small (fast)" },
  { key: "worker", label: "Worker" },
  { key: "reviewer", label: "Reviewer" },
];

// ── Model picker — plain <select>, grouped by provider ───────────────────────
// A settings modal doesn't need ModelSelector's rich search dropdown; a
// native grouped <select> is far less code and perfectly adequate here.

function ModelPicker({
  models,
  value,
  onChange,
  disabled,
}: {
  models: ModelItem[];
  value: string; // "" or "provider:::model"
  onChange: (provider: string, model: string) => void;
  disabled?: boolean;
}) {
  const groups = useMemo(() => {
    const byProvider = new Map<string, ModelItem[]>();
    for (const m of models) {
      const list = byProvider.get(m.providerName) ?? [];
      list.push(m);
      byProvider.set(m.providerName, list);
    }
    return [...byProvider.entries()].sort(([a], [b]) => a.localeCompare(b));
  }, [models]);

  return (
    <select
      value={value}
      disabled={disabled}
      onChange={(e) => {
        const key = e.target.value;
        if (!key) return;
        const idx = key.indexOf(":::");
        if (idx === -1) return;
        onChange(key.slice(0, idx), key.slice(idx + 3));
      }}
      className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text disabled:opacity-40 disabled:cursor-not-allowed"
    >
      <option value="">Choose a model…</option>
      {groups.map(([providerName, items]) => (
        <optgroup key={providerName} label={providerName}>
          {items.map((m) => (
            <option key={m.key} value={m.key}>{m.name}</option>
          ))}
        </optgroup>
      ))}
    </select>
  );
}

// ── One slot row within the System/Folder blocks ─────────────────────────────

function ScopedSlotRow({
  label,
  models,
  slotWire,
  scopeKey, // "global" | "workspace"
  onSet,
  onClear,
  disabled,
}: {
  label: string;
  models: ModelItem[];
  slotWire: ScopedModelSlotWire | undefined;
  scopeKey: "global" | "workspace";
  onSet: (provider: string, model: string) => void;
  onClear: () => void;
  disabled?: boolean;
}) {
  const explicit = scopeKey === "global" ? slotWire?.global : slotWire?.workspace;
  const effective = slotWire?.effective;
  const effectiveScope = slotWire?.effectiveScope;

  return (
    <div className="flex items-center gap-2 py-1.5">
      <span className="text-xs text-text-subtle w-28 shrink-0">{label}</span>
      <div className="flex-1 min-w-0">
        <ModelPicker
          models={models}
          value={explicit ? `${explicit.provider}:::${explicit.model}` : ""}
          onChange={onSet}
          disabled={disabled}
        />
        {!explicit && (
          <p className="text-[10px] text-text-muted mt-0.5 truncate">
            {effective
              ? `inherited: ${effective.provider}/${effective.model}${effectiveScope ? ` (from ${effectiveScope})` : ""}`
              : "not set in any scope"}
          </p>
        )}
      </div>
      {explicit && (
        <button
          onClick={onClear}
          disabled={disabled}
          title={`Clear this ${scopeKey} override`}
          className="shrink-0 p-1.5 rounded-lg text-text-subtle hover:text-red hover:bg-red/10 transition-colors disabled:opacity-40"
        >
          <Undo2 size={13} />
        </button>
      )}
    </div>
  );
}

// ── One slot row within the Session block ─────────────────────────────────────

function SessionSlotRow({
  label,
  slot,
  models,
  session,
  scopedModels,
}: {
  label: string;
  slot: Slot;
  models: ModelItem[];
  session: Session;
  scopedModels: ScopedModelsWire | null;
}) {
  const providerField = `${slot[0].toUpperCase()}${slot.slice(1)}ModelProvider` as keyof Session;
  const idField = `${slot[0].toUpperCase()}${slot.slice(1)}ModelID` as keyof Session;
  const provider = session[providerField] as string;
  const modelID = session[idField] as string;
  const hasOverride = !!(provider && modelID);

  const inherited = scopedModels?.[slot]?.effective;
  const inheritedScope = scopedModels?.[slot]?.effectiveScope;

  return (
    <div className="flex items-center gap-2 py-1.5">
      <span className="text-xs text-text-subtle w-28 shrink-0">{label}</span>
      <div className="flex-1 min-w-0">
        <ModelPicker
          models={models}
          value={hasOverride ? `${provider}:::${modelID}` : ""}
          onChange={(p, m) => setSessionModelSlot(session.ID, slot, p, m)}
        />
        {!hasOverride && (
          <p className="text-[10px] text-text-muted mt-0.5 truncate">
            {inherited
              ? `inherited: ${inherited.provider}/${inherited.model}${inheritedScope ? ` (from ${inheritedScope})` : ""}`
              : "not set in any scope"}
          </p>
        )}
      </div>
      {hasOverride && (
        <button
          onClick={() => clearSessionModelSlot(session.ID, slot)}
          title="Clear this session's override"
          className="shrink-0 p-1.5 rounded-lg text-text-subtle hover:text-red hover:bg-red/10 transition-colors"
        >
          <Undo2 size={13} />
        </button>
      )}
    </div>
  );
}

// setSessionModelSlot writes ONE session model slot (large/small/worker/
// reviewer), leaving every other slot untouched — same nil-means-untouched
// wire convention as clearSessionModelSlot (task #467) and ModelSelector's
// onSelect (task #461).
function setSessionModelSlot(sessionID: string, slot: Slot, provider: string, model: string) {
  ws.send("set_session_models", {
    sessionID,
    [`${slot}Model`]: { provider, model },
  });
}

// ── Main modal ────────────────────────────────────────────────────────────────

export function ScopedModelsModal({ onClose, activeSession }: { onClose: () => void; activeSession: Session | null }) {
  const config = useStore($config);
  const [scopedModels, setScopedModels] = useState<ScopedModelsWire | null>(null);

  const allModels = useMemo(() => buildModelList(config), [config]);
  // Providers without an enabled/API-key-set state are still filtered out by
  // buildProviderGroups already (CLI providers excepted) — reuse the same
  // list ModelSelector shows so this modal never offers a model that can't
  // actually run.
  useMemo(() => buildProviderGroups(config), [config]);

  const refresh = useCallback(() => {
    ws.send("get_scoped_models", {});
  }, []);

  useEffect(() => {
    const unsub = ws.on("scoped_models", (msg: WSMessage) => {
      setScopedModels(msg.payload as ScopedModelsWire);
    });
    refresh();
    return unsub;
  }, [refresh]);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  function setScoped(scope: "global" | "workspace", slot: Slot, provider: string, model: string) {
    ws.send("set_scoped_model", { scope, slot, provider, model });
  }
  function clearScoped(scope: "global" | "workspace", slot: Slot) {
    ws.send("clear_scoped_model", { scope, slot });
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
      onClick={onClose}
      data-test-id="scoped-models-modal-overlay"
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-2xl overflow-hidden flex flex-col max-h-[85vh] chat-font"
        onClick={(e) => e.stopPropagation()}
        data-test-id="scoped-models-modal"
      >
        <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0">
          <div>
            <h2 className="text-base font-semibold text-text">Default models</h2>
            <p className="text-xs text-text-subtle mt-0.5">
              Cascade: System → Folder → Session. An unset slot inherits from the level above.
            </p>
          </div>
          <button onClick={onClose} className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay">
            <X size={16} />
          </button>
        </div>

        <div className="flex-1 overflow-y-auto px-5 py-4 space-y-6">
          {/* System block */}
          <section data-test-id="scoped-models-system">
            <h3 className="text-sm font-semibold text-text mb-1">System</h3>
            <p className="text-[11px] text-text-subtle mb-2">Global default — ~/.local/share/crush/crush.json</p>
            <div className="divide-y divide-surface/30">
              {SLOTS.map(({ key, label }) => (
                <ScopedSlotRow
                  key={key}
                  label={label}
                  models={allModels}
                  slotWire={scopedModels?.[key]}
                  scopeKey="global"
                  onSet={(p, m) => setScoped("global", key, p, m)}
                  onClear={() => clearScoped("global", key)}
                />
              ))}
            </div>
          </section>

          {/* Folder block */}
          <section data-test-id="scoped-models-folder">
            <h3 className="text-sm font-semibold text-text mb-1">Folder</h3>
            <p className="text-[11px] text-text-subtle mb-2">
              Workspace override — ./.crush/crush.json
              {scopedModels && !scopedModels.hasWorkspace && " (no workspace config resolved for this directory)"}
            </p>
            <div className="divide-y divide-surface/30">
              {SLOTS.map(({ key, label }) => (
                <ScopedSlotRow
                  key={key}
                  label={label}
                  models={allModels}
                  slotWire={scopedModels?.[key]}
                  scopeKey="workspace"
                  onSet={(p, m) => setScoped("workspace", key, p, m)}
                  onClear={() => clearScoped("workspace", key)}
                  disabled={!!scopedModels && !scopedModels.hasWorkspace}
                />
              ))}
            </div>
          </section>

          {/* Session block */}
          <section data-test-id="scoped-models-session">
            <h3 className="text-sm font-semibold text-text mb-1">Session</h3>
            {activeSession ? (
              <>
                <p className="text-[11px] text-text-subtle mb-2">Only affects the active session — {activeSession.Title}</p>
                <div className="divide-y divide-surface/30">
                  {SLOTS.map(({ key, label }) => (
                    <SessionSlotRow
                      key={key}
                      label={label}
                      slot={key}
                      models={allModels}
                      session={activeSession}
                      scopedModels={scopedModels}
                    />
                  ))}
                </div>
              </>
            ) : (
              <p className="text-[11px] text-text-subtle">No active session — open a session to set per-session overrides.</p>
            )}
          </section>
        </div>
      </div>
    </div>
  );
}
