import { useState, useRef, useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $sessions, $activeSessionID, $busySessions, setActiveSession, removeSession } from "../store";
import { ws } from "../ws";
import { MessageSquare, Plus, Pencil, X, Check } from "lucide-react";

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
    const confirmed = window.confirm(`Are you sure you want to delete "${title || "Untitled session"}"?`);
    if (!confirmed) return;

    ws.send("delete_session", { sessionID: id });
    removeSession(id);
    if (activeID === id) setActiveSession(null);
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
        <span className="text-xl font-black text-accent tracking-tighter">crush</span>
        <button
          onClick={newSession}
          title="New session"
          className="flex items-center gap-2 px-4 py-2 bg-accent text-white text-sm font-bold rounded-xl hover:bg-accent/90 active:scale-95 transition-all shadow-sm"
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
                    ? "bg-white shadow-sm border border-accent/20"
                    : "hover:bg-white/50 border border-transparent"
                }`}
              >
                <div className="flex items-center gap-3 pr-12">
                  {isBusy && (
                    <span className="w-2 h-2 rounded-full bg-accent shrink-0 animate-pulse" />
                  )}
                  {isEditing ? (
                    <div className="flex-1 flex items-center gap-1.5 min-w-0">
                      <input
                        ref={inputRef}
                        value={editTitle}
                        onChange={(e) => setEditTitle(e.target.value)}
                        onBlur={saveRename}
                        onKeyDown={handleKeyDown}
                        className="text-base font-medium w-full bg-white border border-accent rounded-lg px-2 py-1 outline-none shadow-sm"
                        onClick={(e) => e.stopPropagation()}
                      />
                      <button
                        onClick={(e) => { e.stopPropagation(); saveRename(); }}
                        title="Save"
                        className="w-7 h-7 flex items-center justify-center rounded-lg bg-accent text-white hover:bg-accent/90 shrink-0 shadow-sm"
                      >
                        <Check size={14} />
                      </button>
                      <button
                        onClick={(e) => { e.stopPropagation(); setEditingID(null); }}
                        title="Cancel"
                        className="w-7 h-7 flex items-center justify-center rounded-lg bg-base-overlay text-text-subtle hover:text-text shrink-0 border border-surface"
                      >
                        <X size={14} />
                      </button>
                    </div>
                  ) : (
                    <div className={`text-base font-semibold truncate ${isActive ? "text-accent" : "text-text"}`}>
                      {s.Title || "Untitled session"}
                    </div>
                  )}
                </div>
                {!isEditing && (
                  <div className="flex items-center gap-2.5 mt-1 text-sm text-text-subtle pl-0 font-medium">
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
    </aside>
  );
}
