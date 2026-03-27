import { useState, useEffect } from "react";
import { X, RefreshCw, Download } from "lucide-react";
import { ws } from "../ws";

interface LogsModalProps {
  onClose: () => void;
}

export function LogsModal({ onClose }: LogsModalProps) {
  const [logs, setLogs] = useState<string>("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchLogs = async () => {
    setLoading(true);
    setError(null);

    const msgId = crypto.randomUUID();
    const unsub = ws.on("*", (msg: any) => {
      if (msg.id === msgId) {
        unsub();
        if (msg.type === "logs") {
          setLogs(msg.payload || "");
        } else if (msg.type === "error") {
          setError(msg.error || "Failed to fetch logs");
        }
        setLoading(false);
      }
    });

    ws.send("get_logs", { lines: 1000 }, msgId);
  };

  const handleDownload = () => {
    const blob = new Blob([logs], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `crush-logs-${Date.now()}.txt`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  };

  // Auto-fetch on mount
  useEffect(() => {
    fetchLogs();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Close on Escape
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="fixed inset-0 z-[10000] flex items-center justify-center bg-black/40 backdrop-blur-sm" onClick={onClose}>
      <div className="bg-canvas border border-surface rounded-2xl shadow-2xl w-[min(800px,90vw)] max-h-[80vh] flex flex-col" onClick={e => e.stopPropagation()}>
        {/* Header */}
        <div className="flex items-center justify-between p-4 border-b border-surface">
          <div className="flex items-center gap-2">
            <h2 className="text-lg font-semibold text-text">Logs</h2>
            <span className="text-sm text-text-subtle">
              {logs.split("\n").length} lines
            </span>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={fetchLogs}
              disabled={loading}
              className="p-2 hover:bg-surface rounded-lg transition-colors disabled:opacity-50"
              title="Refresh logs"
            >
              <RefreshCw size={18} className={loading ? "animate-spin" : ""} />
            </button>
            <button
              onClick={handleDownload}
              disabled={!logs || loading}
              className="p-2 hover:bg-surface rounded-lg transition-colors disabled:opacity-50"
              title="Download logs"
            >
              <Download size={18} />
            </button>
            <button
              onClick={onClose}
              className="p-2 hover:bg-surface rounded-lg transition-colors"
            >
              <X size={18} />
            </button>
          </div>
        </div>

        {/* Content */}
        <div className="flex-1 overflow-hidden flex flex-col">
          {error && (
            <div className="p-4 bg-red/10 border border-red/30 rounded-xl m-4">
              <p className="text-red text-sm">{error}</p>
            </div>
          )}

          {loading && (
            <div className="flex items-center justify-center p-8">
              <RefreshCw size={24} className="animate-spin text-text-subtle" />
            </div>
          )}

          {!loading && !error && (
            <pre className="flex-1 overflow-y-auto p-4 text-xs text-text-subtle font-mono bg-surface/30 whitespace-pre-wrap">
              {logs || "No logs available"}
            </pre>
          )}
        </div>
      </div>
    </div>
  );
}
