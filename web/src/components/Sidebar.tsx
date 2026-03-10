import { useState, useRef, useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $sessions, $activeSessionID, $busySessions, setActiveSession, removeSession } from "../store";
import { ws } from "../ws";
import { MessageSquare, Plus, Pencil, X, Check } from "lucide-react";
import { ConfirmDialog } from "./ConfirmDialog";

function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}

export function Sidebar() {
  const sessions = useStore($sessions);
  const activeID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const [editingID, setEditingID] = useState<string | null>(null);
  const [editTitle, setEditTitle] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const [pendingDelete, setPendingDelete] = useState<{ id: string; title: string } | null>(null);

  useEffect(() => {
    if (editingID && inputRef.current) {
      inputRef.current.focus();
      inputRef.current.select();
    }
  }, [editingID]);

  function selectSession(id: string) {
    if (editingID === id) return;
    setActiveSession(id);
    ws.send("load_messages", { sessionID: id });
  }

  function newSession() {
    ws.send("create_session");
  }

  function deleteSession(e: React.MouseEvent, id: string, title: string) {
    e.stopPropagation();
    setPendingDelete({ id, title: title || "Untitled session" });
  }

  function confirmDelete() {
    if (!pendingDelete) return;
    ws.send("delete_session", { sessionID: pendingDelete.id });
    removeSession(pendingDelete.id);
    if (activeID === pendingDelete.id) setActiveSession(null);
    setPendingDelete(null);
  }

  function startEditing(e: React.MouseEvent, id: string, title: string) {
    e.stopPropagation();
    setEditingID(id);
    setEditTitle(title || "Untitled session");
  }

  function saveRename() {
    if (editingID) {
      ws.send("rename_session", { sessionID: editingID, title: editTitle.trim() || "Untitled session" });
      setEditingID(null);
    }
  }

  function handleKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Enter") {
      saveRename();
    } else if (e.key === "Escape") {
      setEditingID(null);
    }
  }

  return (
    <aside className="w-80 bg-base-subtle border-r border-surface flex flex-col overflow-hidden shrink-0">
      {/* Header */}
      <div className="flex items-center justify-between px-6 py-5 border-b border-surface">
        <div className="flex flex-col gap-0.5">
          <span className="text-xl font-black text-accent tracking-tighter">Crush~</span>
          <span className="text-[10px] text-text-subtle font-mono opacity-60">#{__GIT_COUNT__} · {__GIT_COMMIT__}</span>
          <span className="text-[10px] text-text-subtle font-mono opacity-50">{__GIT_BRANCH__}</span>
        </div>
        <button
          onClick={newSession}
          title="New session"
          className="flex items-center gap-2 px-4 py-2 bg-accent-fill text-white/90 text-sm font-bold rounded-xl hover:bg-accent/90 active:scale-95 transition-all shadow-sm"
        >
          <Plus size={18} />
          New
        </button>
      </div>

      {/* Session list */}
      <div className="flex-1 overflow-y-auto py-3 px-3">
        {sessions.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-16 px-6 text-center">
            <div className="w-12 h-12 rounded-2xl bg-base-overlay flex items-center justify-center mb-4 text-text-subtle">
              <MessageSquare size={24} />
            </div>
            <p className="text-text-muted text-base font-semibold">No sessions yet</p>
            <p className="text-text-subtle text-sm mt-1.5">Click New to get started</p>
          </div>
        ) : (
          sessions.map((s) => {
            const isBusy = busySessions.has(s.ID);
            const totalTokens = s.PromptTokens + s.CompletionTokens;
            const isActive = s.ID === activeID;
            const isEditing = editingID === s.ID;

            return (
              <div
                key={s.ID}
                onClick={() => selectSession(s.ID)}
                onDoubleClick={(e) => startEditing(e, s.ID, s.Title)}
                className={`group relative px-4 py-4 rounded-xl cursor-pointer transition-all mb-1 ${
                  isActive
                    ? "bg-canvas shadow-sm border border-accent/20"
                    : "hover:bg-canvas/50 border border-transparent"
                }`}
              >
                <div className={`flex items-start gap-3 ${!isEditing ? "pr-12" : ""}`}>
                  {isBusy && !isEditing && (
                    <span className="w-2 h-2 rounded-full bg-accent shrink-0 animate-pulse mt-2" />
                  )}
                  {isEditing ? (
                    <div className="flex-1 flex flex-col gap-1.5 min-w-0" onClick={(e) => e.stopPropagation()}>
                      <input
                        ref={inputRef}
                        value={editTitle}
                        onChange={(e) => setEditTitle(e.target.value)}
                        onBlur={saveRename}
                        onKeyDown={handleKeyDown}
                        className="font-medium w-full bg-canvas border border-accent rounded-lg px-2 py-1 outline-none shadow-sm"
                        style={{ fontSize: "var(--chat-font-size)" }}
                      />
                      <div className="flex gap-1.5">
                        <button
                          onClick={(e) => { e.stopPropagation(); saveRename(); }}
                          title="Save (Enter)"
                          className="flex items-center gap-1 px-2 py-1 rounded-lg btn-primary text-xs font-semibold"
                        >
                          <Check size={12} /> Save
                        </button>
                        <button
                          onClick={(e) => { e.stopPropagation(); setEditingID(null); }}
                          title="Cancel (Esc)"
                          className="flex items-center gap-1 px-2 py-1 rounded-lg bg-base-overlay text-text-subtle hover:text-text border border-surface text-xs"
                        >
                          <X size={12} /> Cancel
                        </button>
                      </div>
                    </div>
                  ) : (
                    <div className={`font-semibold ${isActive ? "text-accent" : "text-text"}`} style={{ fontSize: "var(--chat-font-size)" }}>
                      {s.Title || "Untitled session"}
                    </div>
                  )}
                </div>
                {!isEditing && (
                  <div className="flex items-center gap-2.5 mt-1 text-text-subtle pl-0 font-medium" style={{ fontSize: "var(--chat-font-size)" }}>
                    <span>{s.MessageCount} msg{s.MessageCount !== 1 ? "s" : ""}</span>
                    {totalTokens > 0 && (
                      <>
                        <span>·</span>
                        <span>{formatTokens(totalTokens)} tok</span>
                      </>
                    )}
                  </div>
                )}

                {!isEditing && (
                  <div className="absolute right-3 top-1/2 -translate-y-1/2 flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity">
                    <button
                      onClick={(e) => startEditing(e, s.ID, s.Title)}
                      title="Rename session"
                      className="w-7 h-7 flex items-center justify-center rounded-lg text-text-subtle hover:text-accent hover:bg-accent/10 transition-colors"
                    >
                      <Pencil size={14} />
                    </button>
                    <button
                      onClick={(e) => deleteSession(e, s.ID, s.Title)}
                      title="Delete session"
                      className="w-7 h-7 flex items-center justify-center rounded-lg text-text-subtle hover:text-red hover:bg-red/10 transition-colors"
                    >
                      <X size={16} />
                    </button>
                  </div>
                )}
              </div>
            );
          })
        )}
      </div>

      {pendingDelete && (
        <ConfirmDialog
          title="Delete session"
          message={`"${pendingDelete.title}" and all its messages will be permanently deleted.`}
          confirmLabel="Delete"
          onConfirm={confirmDelete}
          onCancel={() => setPendingDelete(null)}
        />
      )}
    </aside>
  );
}
