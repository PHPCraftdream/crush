import { useState, useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $config, addCustomProvider, removeCustomProvider, updateCustomProvider } from "../store";
import { X, Plus, Trash2, Pencil, AlertCircle, CheckCircle2, Loader2, ChevronDown, ChevronRight } from "lucide-react";
import type { ProviderInfo } from "../types";

// ── Custom model editor ───────────────────────────────────────────────────────

interface ModelDraft {
  id: string;
  name: string;
  contextWindow: string;
  costPer1mIn: string;
  costPer1mOut: string;
}

function emptyModel(): ModelDraft {
  return { id: "", name: "", contextWindow: "", costPer1mIn: "", costPer1mOut: "" };
}

function ModelEditor({
  models,
  onChange,
}: {
  models: ModelDraft[];
  onChange: (m: ModelDraft[]) => void;
}) {
  function add() { onChange([...models, emptyModel()]); }

  function update(i: number, field: keyof ModelDraft, value: string) {
    const next = [...models];
    next[i] = { ...next[i], [field]: value };
    onChange(next);
  }

  function remove(i: number) { onChange(models.filter((_, idx) => idx !== i)); }

  const inputCls = "w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50";

  return (
    <div className="space-y-2">
      {models.map((m, i) => (
        <div key={i} className="space-y-2 p-3 bg-base-overlay border border-surface rounded-xl">
          <div className="flex items-center justify-between">
            <span className="text-[11px] text-text-subtle font-medium">Model {i + 1}</span>
            {models.length > 1 && (
              <button onClick={() => remove(i)} className="text-text-subtle hover:text-red transition-colors">
                <Trash2 size={12} />
              </button>
            )}
          </div>
          <div className="grid grid-cols-2 gap-2">
            <input value={m.id} onChange={(e) => update(i, "id", e.target.value)} placeholder="ID (e.g. qwen3:30b)" className={inputCls} />
            <input value={m.name} onChange={(e) => update(i, "name", e.target.value)} placeholder="Display name" className={inputCls} />
          </div>
          <div className="grid grid-cols-3 gap-2">
            <input value={m.contextWindow} onChange={(e) => update(i, "contextWindow", e.target.value)} placeholder="Context (tokens)" className={inputCls} />
            <input value={m.costPer1mIn} onChange={(e) => update(i, "costPer1mIn", e.target.value)} placeholder="$/1M in" className={inputCls} />
            <input value={m.costPer1mOut} onChange={(e) => update(i, "costPer1mOut", e.target.value)} placeholder="$/1M out" className={inputCls} />
          </div>
        </div>
      ))}
      <button
        onClick={add}
        className="flex items-center gap-1.5 text-xs text-accent hover:text-accent/80 transition-colors"
      >
        <Plus size={12} />
        Add model
      </button>
    </div>
  );
}

// ── Provider form ─────────────────────────────────────────────────────────────

const PROVIDER_TYPES = ["openai-compat", "openai", "anthropic", "gemini"];

function ProviderForm({
  initial,
  submitLabel,
  onSubmit,
  onCancel,
}: {
  initial?: { id: string; name: string; type: string; baseUrl: string; models: ModelDraft[] };
  submitLabel: string;
  onSubmit: (data: {
    id: string; name: string; type: string; baseUrl: string; apiKey: string; models: ModelDraft[];
  }, msgID: string) => void;
  onCancel: () => void;
}) {
  const [id, setId] = useState(initial?.id ?? "");
  const [name, setName] = useState(initial?.name ?? "");
  const [type, setType] = useState(initial?.type ?? "openai-compat");
  const [baseUrl, setBaseUrl] = useState(initial?.baseUrl ?? "");
  const [apiKey, setApiKey] = useState("");
  const [models, setModels] = useState<ModelDraft[]>(initial?.models ?? [emptyModel()]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function validate(): string | null {
    if (!id.trim()) return "Provider ID is required";
    if (!baseUrl.trim()) return "Base URL is required";
    if (!/^https?:\/\//.test(baseUrl.trim())) return "Base URL must start with http:// or https://";
    if (models.length === 0) return "At least one model is required";
    for (const m of models) {
      if (!m.id.trim()) return "Each model must have an ID";
      if (!m.name.trim()) return "Each model must have a display name";
    }
    return null;
  }

  const canSubmit = !busy &&
    id.trim() !== "" &&
    baseUrl.trim() !== "" &&
    models.length > 0 &&
    models.every((m) => m.id.trim() !== "" && m.name.trim() !== "");

  function submit() {
    const err = validate();
    if (err) { setError(err); return; }
    setError(null);
    setBusy(true);
    const msgID = crypto.randomUUID();
    import("../ws").then(({ ws }) => {
      const unsub = ws.on("*", (msg) => {
        if (msg.id !== msgID) return;
        unsub();
        setBusy(false);
        if (msg.error) setError(msg.error);
        else onCancel();
      });
      onSubmit({ id: id.trim(), name: name.trim(), type, baseUrl: baseUrl.trim(), apiKey, models }, msgID);
    });
  }

  return (
    <div className="border-t border-surface bg-base-subtle/50 p-4 space-y-3">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-semibold text-text">
          {initial ? `Edit: ${initial.id}` : "Add Custom Provider"}
        </h3>
        <button onClick={onCancel} className="text-text-subtle hover:text-text transition-colors">
          <X size={15} />
        </button>
      </div>

      <div className="grid grid-cols-2 gap-2">
        <div>
          <label className="block text-[11px] text-text-subtle mb-1">Provider ID *</label>
          <input
            value={id}
            onChange={(e) => setId(e.target.value)}
            placeholder="e.g. ollama"
            disabled={!!initial}
            className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50 disabled:opacity-60"
          />
        </div>
        <div>
          <label className="block text-[11px] text-text-subtle mb-1">Display Name</label>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. Ollama"
            className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
          />
        </div>
      </div>

      <div>
        <label className="block text-[11px] text-text-subtle mb-1">Base URL *</label>
        <input
          value={baseUrl}
          onChange={(e) => setBaseUrl(e.target.value)}
          placeholder="e.g. http://localhost:11434/v1/"
          className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
        />
      </div>

      <div className="grid grid-cols-2 gap-2">
        <div>
          <label className="block text-[11px] text-text-subtle mb-1">Type</label>
          <select
            value={type}
            onChange={(e) => setType(e.target.value)}
            className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
          >
            {PROVIDER_TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>
        <div>
          <label className="block text-[11px] text-text-subtle mb-1">API Key</label>
          <input
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder={initial ? "Leave blank to keep existing" : "Optional"}
            className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
          />
        </div>
      </div>

      <div>
        <label className="block text-[11px] text-text-subtle mb-2">Models</label>
        <ModelEditor models={models} onChange={setModels} />
      </div>

      {error && (
        <p className="text-xs text-red flex items-center gap-1.5">
          <AlertCircle size={12} />
          {error}
        </p>
      )}
      <div className="flex justify-end gap-2">
        <button
          onClick={onCancel}
          className="px-3 py-1.5 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
        >
          Cancel
        </button>
        <button
          onClick={submit}
          disabled={!canSubmit}
          className="px-4 py-1.5 text-xs font-semibold bg-accent-fill text-white/90 rounded-lg hover:opacity-90 disabled:opacity-40 flex items-center gap-1.5"
        >
          {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}
          {busy ? "Saving…" : submitLabel}
        </button>
      </div>
    </div>
  );
}

// ── Provider row ──────────────────────────────────────────────────────────────

function ProviderRow({
  id,
  info,
  onRemove,
}: {
  id: string;
  info: ProviderInfo;
  onRemove: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [editing, setEditing] = useState(false);

  const models = info.models ?? [];

  if (editing) {
    return (
      <div className="border-b border-surface last:border-0">
        <ProviderForm
          initial={{
            id,
            name: info.name ?? id,
            type: info.type ?? "openai-compat",
            baseUrl: info.baseUrl ?? "",
            models: models.map((m) => ({
              id: m.id,
              name: m.name,
              contextWindow: m.contextWindow ? String(m.contextWindow) : "",
              costPer1mIn: "",
              costPer1mOut: "",
            })),
          }}
          submitLabel="Update Provider"
          onSubmit={(data, msgID) => {
            updateCustomProvider({
              oldId: id,
              id: data.id,
              name: data.name,
              type: data.type,
              baseUrl: data.baseUrl,
              apiKey: data.apiKey || undefined,
              models: data.models.map((m) => ({
                id: m.id,
                name: m.name,
                contextWindow: m.contextWindow ? parseInt(m.contextWindow, 10) : undefined,
                costPer1mIn: m.costPer1mIn ? parseFloat(m.costPer1mIn) : undefined,
                costPer1mOut: m.costPer1mOut ? parseFloat(m.costPer1mOut) : undefined,
              })),
            }, msgID);
          }}
          onCancel={() => setEditing(false)}
        />
      </div>
    );
  }

  return (
    <div className="border-b border-surface last:border-0">
      <div className="flex items-center gap-3 px-4 py-3">
        <button
          onClick={() => models.length > 0 && setExpanded((e) => !e)}
          className={`shrink-0 transition-colors ${models.length > 0 ? "text-text-subtle hover:text-text cursor-pointer" : "text-surface cursor-default"}`}
        >
          {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </button>

        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-sm font-semibold text-text truncate">{info.name || id}</span>
            <span className="text-[11px] font-mono text-text-muted">{id}</span>
            {info.apiKeySet && (
              <span className="inline-flex items-center gap-1 text-[11px] text-green bg-green/10 border border-green/20 rounded-full px-2 py-0.5">
                <CheckCircle2 size={10} />
                Key set
              </span>
            )}
          </div>
          <div className="text-[11px] text-text-subtle font-mono truncate">
            {info.type ?? "openai-compat"}
            {info.baseUrl ? ` · ${info.baseUrl}` : ""}
            {models.length > 0 ? ` · ${models.length} model${models.length !== 1 ? "s" : ""}` : ""}
          </div>
        </div>

        <div className="flex items-center gap-1 shrink-0">
          <button
            onClick={() => setEditing(true)}
            title="Edit provider"
            className="p-1 text-text-subtle hover:text-accent transition-colors rounded"
          >
            <Pencil size={14} />
          </button>
          {confirmRemove ? (
            <div className="flex items-center gap-1 ml-1">
              <span className="text-xs text-text-subtle">Remove?</span>
              <button
                onClick={() => { onRemove(); setConfirmRemove(false); }}
                className="px-2 py-0.5 text-xs font-medium bg-red-fill text-white/90 rounded hover:opacity-90"
              >Yes</button>
              <button
                onClick={() => setConfirmRemove(false)}
                className="px-2 py-0.5 text-xs text-text-subtle hover:text-text rounded"
              >No</button>
            </div>
          ) : (
            <button
              onClick={() => setConfirmRemove(true)}
              title="Remove provider"
              className="p-1 text-text-subtle hover:text-red transition-colors rounded"
            >
              <Trash2 size={14} />
            </button>
          )}
        </div>
      </div>

      {expanded && models.length > 0 && (
        <div className="px-4 pb-3 pl-10">
          <div className="flex flex-wrap gap-1.5">
            {models.map((m) => (
              <span
                key={m.id}
                className="text-[11px] font-mono text-text-muted bg-base-overlay border border-surface rounded-md px-2 py-0.5"
              >
                {m.name || m.id}
              </span>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// ── Providers Modal ───────────────────────────────────────────────────────────

export function ProvidersModal({ onClose }: { onClose: () => void }) {
  const config = useStore($config);
  const [showAdd, setShowAdd] = useState(false);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Only show custom providers (those with isCustom flag).
  const allProviders = Object.entries(config?.providers ?? {});
  const customProviders = allProviders.filter(([, info]) => info.isCustom);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
      onClick={onClose}
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[80vh] chat-font"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0">
          <div>
            <h2 className="text-base font-semibold text-text">Custom Providers</h2>
            <p className="text-xs text-text-subtle mt-0.5">
              {customProviders.length === 0
                ? "No custom providers configured"
                : `${customProviders.length} custom provider${customProviders.length !== 1 ? "s" : ""}`}
            </p>
          </div>
          <button onClick={onClose} className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay">
            <X size={16} />
          </button>
        </div>

        {/* List */}
        <div className="flex-1 overflow-y-auto">
          {customProviders.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-12 px-6 text-center">
              <p className="text-text-muted text-sm font-medium mb-1">No custom providers</p>
              <p className="text-text-subtle text-xs">Add a provider to use Ollama, LM Studio, Deepseek, etc.</p>
            </div>
          ) : (
            customProviders
              .sort(([a], [b]) => a.localeCompare(b))
              .map(([id, info]) => (
                <ProviderRow
                  key={id}
                  id={id}
                  info={info}
                  onRemove={() => removeCustomProvider(id)}
                />
              ))
          )}
        </div>

        {/* Add form */}
        {showAdd ? (
          <ProviderForm
            submitLabel="Add Provider"
            onSubmit={(data, msgID) => {
              addCustomProvider({
                id: data.id,
                name: data.name || undefined,
                type: data.type,
                baseUrl: data.baseUrl,
                apiKey: data.apiKey || undefined,
                models: data.models.map((m) => ({
                  id: m.id,
                  name: m.name,
                  contextWindow: m.contextWindow ? parseInt(m.contextWindow, 10) : undefined,
                  costPer1mIn: m.costPer1mIn ? parseFloat(m.costPer1mIn) : undefined,
                  costPer1mOut: m.costPer1mOut ? parseFloat(m.costPer1mOut) : undefined,
                })),
              }, msgID);
            }}
            onCancel={() => setShowAdd(false)}
          />
        ) : (
          <div className="border-t border-surface px-4 py-3 shrink-0">
            <button
              onClick={() => setShowAdd(true)}
              className="flex items-center gap-2 w-full px-4 py-2.5 text-sm font-medium text-accent border border-accent/30 rounded-xl hover:bg-accent/5 transition-colors"
            >
              <Plus size={15} />
              Add custom provider
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
