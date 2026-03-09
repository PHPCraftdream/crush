import { useState, useEffect, useRef } from "react";
import { useStore } from "@nanostores/react";
import { $mcpState } from "../store";
import { ws } from "../ws";
import {
  X,
  Plus,
  Trash2,
  AlertCircle,
  CheckCircle2,
  Loader2,
  ToggleLeft,
  ToggleRight,
} from "lucide-react";
import type { MCPServerInfo } from "../types";

// ── Status badge ──────────────────────────────────────────────────────────────

function StatusBadge({ info }: { info: MCPServerInfo }) {
  if (info.disabled) {
    return (
      <span className="inline-flex items-center gap-1 text-[11px] font-medium text-text-muted bg-base-overlay border border-surface rounded-full px-2 py-0.5">
        Off
      </span>
    );
  }
  switch (info.status) {
    case "connected":
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-green bg-green/10 border border-green/20 rounded-full px-2 py-0.5">
          <CheckCircle2 size={10} />
          Connected · {info.toolCount} tool{info.toolCount !== 1 ? "s" : ""}
        </span>
      );
    case "starting":
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-accent bg-accent/10 border border-accent/20 rounded-full px-2 py-0.5 animate-pulse">
          <Loader2 size={10} className="animate-spin" />
          Starting…
        </span>
      );
    case "error":
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-red bg-red/10 border border-red/20 rounded-full px-2 py-0.5">
          <AlertCircle size={10} />
          Error
        </span>
      );
    default:
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-text-subtle bg-base-overlay border border-surface rounded-full px-2 py-0.5">
          {info.status}
        </span>
      );
  }
}

// ── Add MCP form ──────────────────────────────────────────────────────────────

const PLACEHOLDER_JSON = `{
  "name": "my-server",
  "type": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
}`;

function AddMCPForm({ onDone }: { onDone: () => void }) {
  const [json, setJson] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [connecting, setConnecting] = useState(false);
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
    if (!name) {
      setError('"name" field is required');
      return;
    }
    const type = (parsed.type as string | undefined) || "stdio";
    if (!["stdio", "http", "sse"].includes(type)) {
      setError('"type" must be "stdio", "http", or "sse"');
      return;
    }

    setConnecting(true);
    const msgID = crypto.randomUUID();
    const unsub = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      unsub();
      setConnecting(false);
      if (msg.error) {
        setError(msg.error);
      } else {
        onDone();
      }
    });

    ws.send("add_mcp_server", {
      name,
      type,
      command: parsed.command as string | undefined,
      args: parsed.args as string[] | undefined,
      url: parsed.url as string | undefined,
      env: parsed.env as Record<string, string> | undefined,
      headers: parsed.headers as Record<string, string> | undefined,
      timeout: parsed.timeout as number | undefined,
    }, msgID);
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Escape") onDone();
  }

  return (
    <div className="border-t border-surface bg-base-subtle/50 p-4">
      <div className="flex items-center justify-between mb-3">
        <h3 className="text-sm font-semibold text-text">Add MCP Server</h3>
        <button onClick={onDone} className="text-text-subtle hover:text-text transition-colors">
          <X size={15} />
        </button>
      </div>
      <p className="text-xs text-text-subtle mb-3 leading-relaxed">
        Paste the server config as JSON. The server will be connected immediately.
      </p>
      <textarea
        ref={taRef}
        value={json}
        onChange={e => setJson(e.target.value)}
        onKeyDown={onKey}
        placeholder={PLACEHOLDER_JSON}
        rows={8}
        spellCheck={false}
        className="w-full font-mono text-xs text-text bg-canvas border border-surface rounded-xl px-3 py-2.5 resize-none outline-none focus:border-accent/50 transition-colors placeholder:text-text-muted/50"
      />
      {error && (
        <p className="text-xs text-red mt-2 flex items-center gap-1.5">
          <AlertCircle size={12} />
          {error}
        </p>
      )}
      <div className="flex justify-end gap-2 mt-3">
        <button
          onClick={onDone}
          className="px-3 py-1.5 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
        >
          Cancel
        </button>
        <button
          onClick={submit}
          disabled={!json.trim() || connecting}
          className="px-4 py-1.5 text-xs font-semibold bg-accent text-white rounded-lg hover:opacity-90 transition-opacity disabled:opacity-40 flex items-center gap-1.5"
        >
          {connecting ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}
          {connecting ? "Connecting…" : "Add Server"}
        </button>
      </div>
    </div>
  );
}

// ── Server row ────────────────────────────────────────────────────────────────

function ServerRow({ info, onRemove }: { info: MCPServerInfo; onRemove: () => void }) {
  const [confirmRemove, setConfirmRemove] = useState(false);

  function toggle() {
    ws.send("set_mcp_disabled", { name: info.name, disabled: !info.disabled });
  }

  return (
    <div className="flex items-center gap-3 px-4 py-3 border-b border-surface last:border-0">
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold text-text truncate">{info.name}</span>
          <StatusBadge info={info} />
        </div>
      </div>

      <div className="flex items-center gap-1 shrink-0">
        {/* Toggle */}
        <button
          onClick={toggle}
          title={info.disabled ? "Enable" : "Disable"}
          className={`transition-colors ${info.disabled ? "text-text-subtle hover:text-accent" : "text-accent hover:text-accent/70"}`}
        >
          {info.disabled
            ? <ToggleLeft size={22} />
            : <ToggleRight size={22} />}
        </button>

        {/* Remove */}
        {confirmRemove ? (
          <div className="flex items-center gap-1 ml-1">
            <span className="text-xs text-text-subtle">Remove?</span>
            <button
              onClick={() => { onRemove(); setConfirmRemove(false); }}
              className="px-2 py-0.5 text-xs font-medium bg-red text-white rounded hover:opacity-90 transition-opacity"
            >Yes</button>
            <button
              onClick={() => setConfirmRemove(false)}
              className="px-2 py-0.5 text-xs text-text-subtle hover:text-text transition-colors rounded"
            >No</button>
          </div>
        ) : (
          <button
            onClick={() => setConfirmRemove(true)}
            title="Remove server"
            className="p-1 text-text-subtle hover:text-red transition-colors rounded"
          >
            <Trash2 size={14} />
          </button>
        )}
      </div>
    </div>
  );
}

// ── MCP Settings Modal ────────────────────────────────────────────────────────

export function MCPSettings({ onClose }: { onClose: () => void }) {
  const mcpState = useStore($mcpState);
  const [showAdd, setShowAdd] = useState(false);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  function removeServer(name: string) {
    ws.send("remove_mcp_server", { name });
  }

  const servers = mcpState?.servers ?? [];
  const sorted = [...servers].sort((a, b) => a.name.localeCompare(b.name));

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
      onClick={onClose}
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[80vh]"
        onClick={e => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0">
          <div>
            <h2 className="text-base font-semibold text-text">MCP Servers</h2>
            <p className="text-xs text-text-subtle mt-0.5">
              {servers.length === 0
                ? "No servers configured"
                : `${servers.filter(s => !s.disabled).length} of ${servers.length} enabled`}
            </p>
          </div>
          <button onClick={onClose} className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay">
            <X size={16} />
          </button>
        </div>

        {/* Server list */}
        <div className="flex-1 overflow-y-auto">
          {sorted.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-12 px-6 text-center">
              <p className="text-text-muted text-sm font-medium mb-1">No MCP servers</p>
              <p className="text-text-subtle text-xs">Add a server to get started</p>
            </div>
          ) : (
            sorted.map(info => (
              <ServerRow
                key={info.name}
                info={info}
                onRemove={() => removeServer(info.name)}
              />
            ))
          )}
        </div>

        {/* Add server */}
        {showAdd ? (
          <AddMCPForm onDone={() => setShowAdd(false)} />
        ) : (
          <div className="border-t border-surface px-4 py-3 shrink-0">
            <button
              onClick={() => setShowAdd(true)}
              className="flex items-center gap-2 w-full px-4 py-2.5 text-sm font-medium text-accent border border-accent/30 rounded-xl hover:bg-accent/5 transition-colors"
            >
              <Plus size={15} />
              Add MCP server
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
