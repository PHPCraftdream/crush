import { useState, useEffect, useRef } from "react";
import { useStore } from "@nanostores/react";
import { $lspSnapshot } from "../store";
import { ws } from "../ws";
import {
  X, Plus, Trash2, AlertCircle, CheckCircle2, Loader2,
  ToggleLeft, ToggleRight, ChevronDown, ChevronRight, Pencil, FileCode,
} from "lucide-react";
import type { LSPServerInfo } from "../types";

function StatusBadge({ info }: { info: LSPServerInfo }) {
  if (info.disabled) {
    return <span className="inline-flex items-center gap-1 text-[11px] font-medium text-text-muted bg-base-overlay border border-surface rounded-full px-2 py-0.5">Off</span>;
  }
  switch (info.state) {
    case "ready":
      return <span className="inline-flex items-center gap-1 text-[11px] font-medium text-green bg-green/10 border border-green/20 rounded-full px-2 py-0.5"><CheckCircle2 size={10} />Ready{info.diagnosticCount > 0 ? ` · ${info.diagnosticCount} diag` : ""}</span>;
    case "starting":
      return <span className="inline-flex items-center gap-1 text-[11px] font-medium text-accent bg-accent/10 border border-accent/20 rounded-full px-2 py-0.5 animate-pulse"><Loader2 size={10} className="animate-spin" />Starting…</span>;
    case "error":
      return <span className="inline-flex items-center gap-1 text-[11px] font-medium text-red bg-red/10 border border-red/20 rounded-full px-2 py-0.5"><AlertCircle size={10} />Error</span>;
    case "unstarted":
    case "stopped":
      return <span className="inline-flex items-center gap-1 text-[11px] font-medium text-text-subtle bg-base-overlay border border-surface rounded-full px-2 py-0.5">Idle</span>;
    default:
      return <span className="inline-flex items-center gap-1 text-[11px] font-medium text-text-subtle bg-base-overlay border border-surface rounded-full px-2 py-0.5">{info.state}</span>;
  }
}

const PLACEHOLDER_JSON = `{
  "name": "gopls",
  "command": "gopls",
  "fileTypes": ["go", "mod"]
}`;

function buildInitialJson(info: LSPServerInfo): string {
  const obj: Record<string, unknown> = { name: info.name };
  if (info.command) obj.command = info.command;
  if (info.args?.length) obj.args = info.args;
  if (info.fileTypes?.length) obj.fileTypes = info.fileTypes;
  if (info.env && Object.keys(info.env).length > 0) obj.env = info.env;
  return JSON.stringify(obj, null, 2);
}

function LSPForm({ initial, submitLabel, onSubmit, onCancel }: {
  initial?: LSPServerInfo;
  submitLabel: string;
  onSubmit: (parsed: Record<string, unknown>, msgID: string) => void;
  onCancel: () => void;
}) {
  const [json, setJson] = useState(() => initial ? buildInitialJson(initial) : "");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const taRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => { taRef.current?.focus(); }, []);

  function submit() {
    setError(null);
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(json.trim());
    } catch {
      setError("Invalid JSON — check your syntax");
      return;
    }
    const name = (parsed.name as string | undefined)?.trim();
    if (!name) { setError('"name" field is required'); return; }
    const command = (parsed.command as string | undefined)?.trim();
    if (!command) { setError('"command" field is required'); return; }

    setBusy(true);
    const msgID = crypto.randomUUID();
    const unsub = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      unsub();
      setBusy(false);
      if (msg.error) setError(msg.error);
      else onCancel();
    });
    onSubmit(parsed, msgID);
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Escape") onCancel();
  }

  return (
    <div className="border-t border-surface bg-base-subtle/50 p-4">
      <div className="flex items-center justify-between mb-3">
        <h3 className="text-sm font-semibold text-text">{initial ? `Edit: ${initial.name}` : "Add LSP Server"}</h3>
        <button onClick={onCancel} className="text-text-subtle hover:text-text transition-colors"><X size={15} /></button>
      </div>
      {!initial && (
        <p className="text-xs text-text-subtle mb-3 leading-relaxed">
          Paste the server config as JSON. The server will start when a matching file is opened.
        </p>
      )}
      <textarea
        ref={taRef}
        value={json}
        onChange={e => setJson(e.target.value)}
        onKeyDown={onKey}
        placeholder={initial ? undefined : PLACEHOLDER_JSON}
        rows={8}
        spellCheck={false}
        className="w-full font-mono text-xs text-text bg-canvas border border-surface rounded-xl px-3 py-2.5 resize-none outline-none focus:border-accent/50 transition-colors placeholder:text-text-muted/50"
      />
      {error && <p className="text-xs text-red mt-2 flex items-center gap-1.5"><AlertCircle size={12} />{error}</p>}
      <div className="flex justify-end gap-2 mt-3">
        <button onClick={onCancel} className="px-3 py-1.5 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay">Cancel</button>
        <button
          onClick={submit}
          disabled={!json.trim() || busy}
          className="px-4 py-1.5 text-xs font-semibold bg-accent-fill text-white/90 rounded-lg hover:opacity-90 transition-opacity disabled:opacity-40 flex items-center gap-1.5"
        >
          {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}
          {busy ? "Saving…" : submitLabel}
        </button>
      </div>
    </div>
  );
}

function ServerRow({ info, onRemove }: { info: LSPServerInfo; onRemove: () => void }) {
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [editing, setEditing] = useState(false);

  function toggle() {
    ws.send("set_lsp_disabled", { name: info.name, disabled: !info.disabled });
  }

  if (editing) {
    return (
      <div className="border-b border-surface last:border-0">
        <LSPForm
          initial={info}
          submitLabel="Update Server"
          onSubmit={(parsed, msgID) => {
            ws.send("update_lsp_server", {
              oldName: info.name,
              name: (parsed.name as string).trim(),
              command: (parsed.command as string).trim(),
              args: parsed.args as string[] | undefined,
              fileTypes: parsed.fileTypes as string[] | undefined,
              env: parsed.env as Record<string, string> | undefined,
              timeout: parsed.timeout as number | undefined,
            }, msgID);
          }}
          onCancel={() => setEditing(false)}
        />
      </div>
    );
  }

  const hasFileTypes = (info.fileTypes?.length ?? 0) > 0;

  return (
    <div className="border-b border-surface last:border-0">
      <div className="flex items-center gap-3 px-4 py-3">
        <button
          onClick={() => hasFileTypes && setExpanded(e => !e)}
          className={`shrink-0 transition-colors ${hasFileTypes ? "text-text-subtle hover:text-text cursor-pointer" : "text-surface cursor-default"}`}
          title={hasFileTypes ? (expanded ? "Hide file types" : "Show file types") : "No file types configured"}
        >
          {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </button>

        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-sm font-semibold text-text truncate">{info.name}</span>
            <StatusBadge info={info} />
          </div>
          {info.command && (
            <span className="text-[11px] text-text-subtle font-mono">{info.command}{info.args?.length ? ` ${info.args.join(" ")}` : ""}</span>
          )}
        </div>

        <div className="flex items-center gap-1 shrink-0">
          <button onClick={() => setEditing(true)} title="Edit server" className="p-1 text-text-subtle hover:text-accent transition-colors rounded"><Pencil size={14} /></button>
          <button
            onClick={toggle}
            title={info.disabled ? "Enable" : "Disable"}
            className={`transition-colors ${info.disabled ? "text-text-subtle hover:text-accent" : "text-accent hover:text-accent/70"}`}
          >
            {info.disabled ? <ToggleLeft size={22} /> : <ToggleRight size={22} />}
          </button>
          {confirmRemove ? (
            <div className="flex items-center gap-1 ml-1">
              <span className="text-xs text-text-subtle">Remove?</span>
              <button onClick={() => { onRemove(); setConfirmRemove(false); }} className="px-2 py-0.5 text-xs font-medium bg-red-fill text-white/90 rounded hover:opacity-90 transition-opacity">Yes</button>
              <button onClick={() => setConfirmRemove(false)} className="px-2 py-0.5 text-xs text-text-subtle hover:text-text transition-colors rounded">No</button>
            </div>
          ) : (
            <button onClick={() => setConfirmRemove(true)} title="Remove server" className="p-1 text-text-subtle hover:text-red transition-colors rounded"><Trash2 size={14} /></button>
          )}
        </div>
      </div>

      {expanded && hasFileTypes && (
        <div className="px-4 pb-3 pl-10">
          <div className="flex items-center gap-1.5 mb-2">
            <FileCode size={11} className="text-text-subtle" />
            <span className="text-[11px] font-semibold text-text-subtle uppercase tracking-wider">File Types</span>
          </div>
          <div className="flex flex-wrap gap-1.5">
            {info.fileTypes!.map(ft => (
              <span key={ft} className="text-[11px] font-mono text-text-muted bg-base-overlay border border-surface rounded-md px-2 py-0.5">{ft}</span>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

export function LSPSettings({ onClose }: { onClose: () => void }) {
  const lspSnapshot = useStore($lspSnapshot);
  const [showAdd, setShowAdd] = useState(false);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  function removeServer(name: string) {
    ws.send("remove_lsp_server", { name });
  }

  const servers = lspSnapshot?.servers ?? [];
  const sorted = [...servers].sort((a, b) => a.name.localeCompare(b.name));

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4" onClick={onClose}>
      <div className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[80vh] chat-font" onClick={e => e.stopPropagation()}>
        <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0">
          <div>
            <h2 className="text-base font-semibold text-text">LSP Servers</h2>
            <p className="text-xs text-text-subtle mt-0.5">
              {servers.length === 0 ? "No servers configured" : `${servers.filter(s => !s.disabled).length} of ${servers.length} enabled`}
            </p>
          </div>
          <button onClick={onClose} className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay"><X size={16} /></button>
        </div>

        <div className="flex-1 overflow-y-auto">
          {sorted.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-12 px-6 text-center">
              <p className="text-text-muted text-sm font-medium mb-1">No LSP servers</p>
              <p className="text-text-subtle text-xs">Add a server to get started</p>
            </div>
          ) : (
            sorted.map(info => <ServerRow key={info.name} info={info} onRemove={() => removeServer(info.name)} />)
          )}
        </div>

        {showAdd ? (
          <LSPForm
            submitLabel="Add Server"
            onSubmit={(parsed, msgID) => {
              ws.send("add_lsp_server", {
                name: (parsed.name as string).trim(),
                command: (parsed.command as string).trim(),
                args: parsed.args as string[] | undefined,
                fileTypes: parsed.fileTypes as string[] | undefined,
                env: parsed.env as Record<string, string> | undefined,
                timeout: parsed.timeout as number | undefined,
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
              Add LSP server
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
