import { useState, useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $config, addCustomProvider, removeCustomProvider, updateCustomProvider, type ConfigScope } from "../store";
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

// ── Peak-hours helper ────────────────────────────────────────────────────────

// isPeakHoursActive returns true when the browser's local time-of-day falls
// inside the [start, end) window. Handles overnight wrap (start > end), e.g.
// "22:00"–"06:00". Returns false when the window is malformed/empty.
const HH_MM_RE = /^([01]\d|2[0-3]):[0-5]\d$/;

function isPeakHoursActive(peak: { start: string; end: string } | null | undefined): boolean {
  if (!peak || !peak.start || !peak.end) return false;
  const now = new Date();
  const cur = now.getHours() * 60 + now.getMinutes();
  const [sh, sm] = peak.start.split(":").map(Number);
  const [eh, em] = peak.end.split(":").map(Number);
  if ([sh, sm, eh, em].some((n) => Number.isNaN(n))) return false;
  const start = sh * 60 + sm;
  const end = eh * 60 + em;
  return start <= end ? cur >= start && cur < end : cur >= start || cur < end;
}

// ── Provider form ─────────────────────────────────────────────────────────────

const PROVIDER_TYPES = ["openai-compat", "openai", "anthropic", "gemini"];

function ProviderForm({
  initial,
  submitLabel,
  onSubmit,
  onCancel,
}: {
  initial?: { id: string; name: string; type: string; baseUrl: string; models: ModelDraft[]; peakHours?: { start: string; end: string } | null };
  submitLabel: string;
  onSubmit: (data: {
    id: string; name: string; type: string; baseUrl: string; apiKey: string; models: ModelDraft[];
    peakHours: { start: string; end: string } | null; scope: ConfigScope;
  }, msgID: string) => void;
  onCancel: () => void;
}) {
  const [id, setId] = useState(initial?.id ?? "");
  const [name, setName] = useState(initial?.name ?? "");
  const [type, setType] = useState(initial?.type ?? "openai-compat");
  const [baseUrl, setBaseUrl] = useState(initial?.baseUrl ?? "");
  const [apiKey, setApiKey] = useState("");
  const [models, setModels] = useState<ModelDraft[]>(initial?.models ?? [emptyModel()]);
  const [peakEnabled, setPeakEnabled] = useState(!!initial?.peakHours?.start && !!initial?.peakHours?.end);
  const [peakStart, setPeakStart] = useState(initial?.peakHours?.start ?? "09:00");
  const [peakEnd, setPeakEnd] = useState(initial?.peakHours?.end ?? "18:00");
  // Default global — matches every scope-aware CLI command's default
  // (crush providers, crush mcp, crush claude-init, ...). There is no
  // read-back of an existing provider's current scope (would require a
  // separate per-scope config read the server doesn't expose yet), so
  // editing always starts from "global"; pick "local" explicitly if the
  // provider actually lives in the workspace config.
  const [scope, setScope] = useState<ConfigScope>("global");
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
    if (peakEnabled && (!peakStart || !peakEnd)) return "Peak-hours start and end are required";
    if (peakEnabled && (!HH_MM_RE.test(peakStart) || !HH_MM_RE.test(peakEnd))) return "Peak-hours must be in 24-hour HH:MM format";
    return null;
  }

  const canSubmit = !busy &&
    id.trim() !== "" &&
    baseUrl.trim() !== "" &&
    models.length > 0 &&
    models.every((m) => m.id.trim() !== "" && m.name.trim() !== "") &&
    (!peakEnabled || (HH_MM_RE.test(peakStart) && HH_MM_RE.test(peakEnd)));

  const peakHours = peakEnabled && peakStart && peakEnd ? { start: peakStart, end: peakEnd } : null;

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
      onSubmit({ id: id.trim(), name: name.trim(), type, baseUrl: baseUrl.trim(), apiKey, models, peakHours, scope }, msgID);
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
        <label className="block text-[11px] text-text-subtle mb-1">Scope</label>
        <div className="flex gap-1 p-0.5 bg-canvas border border-surface rounded-lg w-fit" data-test-id="provider-form-scope">
          {(["global", "local"] as const).map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => setScope(s)}
              data-test-id={`provider-form-scope-${s}`}
              className={`px-3 py-1 text-xs rounded-md transition-colors capitalize ${
                scope === s ? "bg-accent-fill text-white/90" : "text-text-subtle hover:text-text"
              }`}
            >
              {s}
            </button>
          ))}
        </div>
        <p className="text-[10px] text-text-muted mt-1">
          {scope === "global" ? "Available in every project (~/.local/share/crush)." : "This project only (./.crush)."}
        </p>
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
            // Native <select>/<option> popups are rendered by the OS/browser
            // chrome, not by our stylesheet — Tailwind's bg-canvas class
            // alone leaves the dropdown list translucent on some platforms.
            // Force an explicit opaque background inline so the popup
            // itself (not just the closed control) is solid.
            style={{ backgroundColor: "rgb(var(--color-canvas))", color: "rgb(var(--color-text))" }}
            className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
          >
            {PROVIDER_TYPES.map((t) => (
              <option
                key={t}
                value={t}
                style={{ backgroundColor: "rgb(var(--color-canvas))", color: "rgb(var(--color-text))" }}
              >
                {t}
              </option>
            ))}
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

      <div className="space-y-2 border-t border-surface pt-3">
        <label className="flex items-center gap-2 text-[11px] text-text-subtle cursor-pointer select-none">
          <input
            type="checkbox"
            checked={peakEnabled}
            onChange={(e) => setPeakEnabled(e.target.checked)}
            className="accent-accent"
            data-test-id="provider-form-peak-toggle"
          />
          Restrict to hours
        </label>
        {peakEnabled && (
          <div className="grid grid-cols-2 gap-2" data-test-id="provider-form-peak-inputs">
            <div>
              <label className="block text-[11px] text-text-subtle mb-1">Start (local)</label>
              <input
                type="text"
                // A plain HH:MM text field, not <input type="time"> — the
                // native time picker's AM/PM vs 24h display follows the OS
                // locale in most non-Chromium browsers (lang="en-GB" doesn't
                // override it there), but peak_hours is always 24h HH:MM
                // server-side, so we own the format instead of fighting the
                // picker.
                inputMode="numeric"
                pattern="([01]\d|2[0-3]):[0-5]\d"
                placeholder="HH:MM"
                maxLength={5}
                value={peakStart}
                onChange={(e) => setPeakStart(e.target.value)}
                className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
                data-test-id="provider-form-peak-start"
              />
            </div>
            <div>
              <label className="block text-[11px] text-text-subtle mb-1">End (local)</label>
              <input
                type="text"
                inputMode="numeric"
                pattern="([01]\d|2[0-3]):[0-5]\d"
                placeholder="HH:MM"
                maxLength={5}
                value={peakEnd}
                onChange={(e) => setPeakEnd(e.target.value)}
                className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
                data-test-id="provider-form-peak-end"
              />
            </div>
          </div>
        )}
        {peakEnabled && (
          <p className="text-[10px] text-text-muted">
            {isPeakHoursActive(peakHours) ? "Active now" : "Outside window"} · overnight windows supported
          </p>
        )}
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

// ── Peak-hours-only editor (built-in providers) ───────────────────────────────
//
// Built-in/catwalk-known providers (anthropic, openai, zai, ...) have a
// fixed type/baseUrl/model-list the client doesn't own — ProviderForm's full
// add/edit flow (which replaces every field) is only safe for custom
// providers. This editor sends targeted single-field writes instead
// (set_provider_peak_hours, set_provider_key/remove_provider_key), so a
// built-in provider's API key and peak_hours can be managed without
// touching anything else about it (type, base URL, model list).
function BuiltinProviderEditor({
  id,
  initial,
  apiKeySet,
  onCancel,
}: {
  id: string;
  initial: { start: string; end: string } | null | undefined;
  apiKeySet?: boolean;
  onCancel: () => void;
}) {
  const [enabled, setEnabled] = useState(!!initial?.start && !!initial?.end);
  const [start, setStart] = useState(initial?.start ?? "09:00");
  const [end, setEnd] = useState(initial?.end ?? "18:00");
  const [scope, setScope] = useState<ConfigScope>("global");
  const [apiKey, setApiKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const canSubmit = !busy && (!enabled || (HH_MM_RE.test(start) && HH_MM_RE.test(end)));

  function sendAndWait(type: string, payload: Record<string, unknown>): Promise<string | null> {
    return new Promise((resolve) => {
      const msgID = crypto.randomUUID();
      import("../ws").then(({ ws }) => {
        const unsub = ws.on("*", (msg) => {
          if (msg.id !== msgID) return;
          unsub();
          resolve(msg.error ?? null);
        });
        ws.send(type, payload, msgID);
      });
    });
  }

  async function submit() {
    if (enabled && (!start || !end)) { setError("Start and end are required"); return; }
    if (enabled && (!HH_MM_RE.test(start) || !HH_MM_RE.test(end))) { setError("Must be in 24-hour HH:MM format"); return; }
    setError(null);
    setBusy(true);
    const errs = await Promise.all([
      sendAndWait("set_provider_peak_hours", { id, peakHours: enabled ? { start, end } : null, scope }),
      ...(apiKey.trim() ? [sendAndWait("set_provider_key", { providerID: id, apiKey: apiKey.trim() })] : []),
    ]);
    setBusy(false);
    const err = errs.find((e) => e);
    if (err) setError(err);
    else onCancel();
  }

  async function removeKey() {
    setBusy(true);
    const err = await sendAndWait("remove_provider_key", { providerID: id });
    setBusy(false);
    if (err) setError(err);
  }

  return (
    <div className="border-t border-surface bg-base-subtle/50 p-4 space-y-3" data-test-id="builtin-provider-editor">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-semibold text-text">Edit: {id}</h3>
        <button onClick={onCancel} className="text-text-subtle hover:text-text transition-colors">
          <X size={15} />
        </button>
      </div>

      <div>
        <label className="block text-[11px] text-text-subtle mb-1">API key</label>
        <div className="flex gap-2">
          <input
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder={apiKeySet ? "•••••••• (leave blank to keep)" : "sk-…"}
            className="flex-1 text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
            data-test-id="peak-hours-only-api-key"
          />
          {apiKeySet && (
            <button
              onClick={removeKey}
              disabled={busy}
              title="Remove API key"
              className="px-2.5 py-1.5 text-xs text-text-subtle hover:text-red transition-colors rounded-lg border border-surface disabled:opacity-40"
              data-test-id="peak-hours-only-remove-key"
            >
              Remove
            </button>
          )}
        </div>
      </div>

      <div>
        <label className="block text-[11px] text-text-subtle mb-1">Peak-hours scope</label>
        <div className="flex gap-1 p-0.5 bg-canvas border border-surface rounded-lg w-fit" data-test-id="peak-hours-only-scope">
          {(["global", "local"] as const).map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => setScope(s)}
              data-test-id={`peak-hours-only-scope-${s}`}
              className={`px-3 py-1 text-xs rounded-md transition-colors capitalize ${
                scope === s ? "bg-accent-fill text-white/90" : "text-text-subtle hover:text-text"
              }`}
            >
              {s}
            </button>
          ))}
        </div>
      </div>

      <label className="flex items-center gap-2 text-[11px] text-text-subtle cursor-pointer select-none">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => setEnabled(e.target.checked)}
          className="accent-accent"
          data-test-id="peak-hours-only-toggle"
        />
        Restrict to hours
      </label>
      {enabled && (
        <div className="grid grid-cols-2 gap-2" data-test-id="peak-hours-only-inputs">
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">Start (local)</label>
            <input
              type="text"
              // Plain HH:MM text field — see the comment on the equivalent
              // input in ProviderForm above for why not <input type="time">.
              inputMode="numeric"
              pattern="([01]\d|2[0-3]):[0-5]\d"
              placeholder="HH:MM"
              maxLength={5}
              value={start}
              onChange={(e) => setStart(e.target.value)}
              className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
              data-test-id="peak-hours-only-start"
            />
          </div>
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">End (local)</label>
            <input
              type="text"
              inputMode="numeric"
              pattern="([01]\d|2[0-3]):[0-5]\d"
              placeholder="HH:MM"
              maxLength={5}
              value={end}
              onChange={(e) => setEnd(e.target.value)}
              className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
              data-test-id="peak-hours-only-end"
            />
          </div>
        </div>
      )}

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
          data-test-id="peak-hours-only-save"
        >
          {busy ? <Loader2 size={12} className="animate-spin" /> : null}
          {busy ? "Saving…" : "Save"}
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
  onRemove: (scope: ConfigScope) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [editing, setEditing] = useState(false);

  const models = info.models ?? [];
  const isCustom = !!info.isCustom;

  if (editing && !isCustom) {
    return (
      <div className="border-b border-surface last:border-0">
        <BuiltinProviderEditor id={id} initial={info.peakHours} apiKeySet={info.apiKeySet} onCancel={() => setEditing(false)} />
      </div>
    );
  }

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
            peakHours: info.peakHours ?? null,
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
              // Send null explicitly when cleared so the server clears it.
              peakHours: data.peakHours,
              scope: data.scope,
            }, msgID);
          }}
          onCancel={() => setEditing(false)}
        />
      </div>
    );
  }

  return (
    <div className="border-b border-surface last:border-0" data-test-id="provider-row" data-provider-id={id}>
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
            {info.peakHours && info.peakHours.start && info.peakHours.end && (
              <span
                className="inline-flex items-center gap-1 text-[11px] font-mono bg-accent/10 border border-accent/20 rounded-full px-2 py-0.5"
                data-test-id="provider-peak-badge"
                title={isPeakHoursActive(info.peakHours) ? "Currently in peak-hours window" : "Outside peak-hours window"}
              >
                {info.peakHours.start}–{info.peakHours.end}
                {isPeakHoursActive(info.peakHours) && (
                  <span className="inline-block w-1.5 h-1.5 rounded-full bg-accent animate-pulse" />
                )}
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
            data-test-id={`provider-edit-${id}`}
            className="p-1 text-text-subtle hover:text-accent transition-colors rounded"
          >
            <Pencil size={14} />
          </button>
          {isCustom && (confirmRemove ? (
            <div className="flex items-center gap-1 ml-1">
              <span className="text-xs text-text-subtle">Remove from:</span>
              <button
                onClick={() => { onRemove("global"); setConfirmRemove(false); }}
                title="Remove the global (~/.local/share/crush) override"
                className="px-2 py-0.5 text-xs font-medium bg-red-fill text-white/90 rounded hover:opacity-90"
                data-test-id="provider-remove-global"
              >Global</button>
              <button
                onClick={() => { onRemove("local"); setConfirmRemove(false); }}
                title="Remove the local (./.crush) override"
                className="px-2 py-0.5 text-xs font-medium bg-red-fill/70 text-white/90 rounded hover:opacity-90"
                data-test-id="provider-remove-local"
              >Local</button>
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
          ))}
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

  // Show every configured provider — built-in/catwalk-known (anthropic,
  // openai, zai, ...) as well as custom. Built-in providers only get the
  // peak-hours-only editor + no remove button (see ProviderRow); full
  // add/edit/remove stays custom-provider-only.
  const allProviders = Object.entries(config?.providers ?? {});

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
      onClick={onClose}
      data-test-id="providers-modal-overlay"
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[80vh] chat-font"
        onClick={(e) => e.stopPropagation()}
        data-test-id="providers-modal"
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0" data-test-id="providers-modal-header">
          <div>
            <h2 className="text-base font-semibold text-text">Providers</h2>
            <p className="text-xs text-text-subtle mt-0.5">
              {allProviders.length === 0
                ? "No providers configured"
                : `${allProviders.length} provider${allProviders.length !== 1 ? "s" : ""}`}
            </p>
          </div>
          <button onClick={onClose} className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay">
            <X size={16} />
          </button>
        </div>

        {/* List */}
        <div className="flex-1 overflow-y-auto">
          {allProviders.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-12 px-6 text-center">
              <p className="text-text-muted text-sm font-medium mb-1">No providers</p>
              <p className="text-text-subtle text-xs">Add a provider to use Ollama, LM Studio, Deepseek, etc.</p>
            </div>
          ) : (
            allProviders
              // Configured providers (API key set) first, then everything
              // else — alphabetical within each group.
              .sort(([a, infoA], [b, infoB]) => {
                const configuredA = infoA.apiKeySet ? 0 : 1;
                const configuredB = infoB.apiKeySet ? 0 : 1;
                if (configuredA !== configuredB) return configuredA - configuredB;
                return a.localeCompare(b);
              })
              .map(([id, info]) => (
                <ProviderRow
                  key={id}
                  id={id}
                  info={info}
                  onRemove={(scope) => removeCustomProvider(id, scope)}
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
                peakHours: data.peakHours,
                scope: data.scope,
              }, msgID);
            }}
            onCancel={() => setShowAdd(false)}
          />
        ) : (
          <div className="border-t border-surface px-4 py-3 shrink-0">
            <button
              onClick={() => setShowAdd(true)}
              className="flex items-center gap-2 w-full px-4 py-2.5 text-sm font-medium text-accent border border-accent/30 rounded-xl hover:bg-accent/5 transition-colors"
              data-test-id="providers-modal-add"
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
