import { useEffect, useRef, useState, useCallback, useMemo } from "react";
import { useStore } from "@nanostores/react";
import {
  $messages,
  $activeSessionID,
  $sessions,
  $busySessions,
  $agentError,
  $selectedMessageIDs,
  $messageQueue,
  clearSelection,
  toggleMessageSelection,
  deleteMessage,
  deleteMessages,
  selectMessageIDs,
  removeQueuedMessage,
  updateQueuedMessage,
  type QueuedMessage,
} from "../store";
import { ws } from "../ws";
import { Message } from "./Message";
import { ChatInput } from "./ChatInput";
import { PermissionDialog } from "./PermissionDialog";
import { ConfirmDialog } from "./ConfirmDialog";
import { ChatToolbar } from "./ChatToolbar";
import { TodoList } from "./TodoList";
import { MessageSquare, Pencil, Sparkles, Square, Trash2, X } from "lucide-react";

// ── Queued message item ───────────────────────────────────────────────────────

function QueuedMessageItem({
  item,
  sessionID,
  position,
  total,
}: {
  item: QueuedMessage;
  sessionID: string;
  position: number;
  total: number;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const taRef = useRef<HTMLTextAreaElement>(null);

  const startEdit = useCallback(() => { setDraft(item.content); setEditing(true); }, [item.content]);

  useEffect(() => {
    if (editing && taRef.current) {
      taRef.current.focus();
      taRef.current.selectionStart = taRef.current.value.length;
      taRef.current.style.height = "auto";
      taRef.current.style.height = taRef.current.scrollHeight + "px";
    }
  }, [editing]);

  const save = useCallback(() => {
    const trimmed = draft.trim();
    if (trimmed) updateQueuedMessage(sessionID, item.id, trimmed);
    setEditing(false);
  }, [draft, sessionID, item.id]);

  const onKey = useCallback((e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Escape") setEditing(false);
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) save();
  }, [save]);

  const onInput = useCallback((e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setDraft(e.target.value);
    e.target.style.height = "auto";
    e.target.style.height = e.target.scrollHeight + "px";
  }, []);

  const handleRemove = useCallback(() => removeQueuedMessage(sessionID, item.id), [sessionID, item.id]);

  return (
    <div className="group/qi flex justify-end px-8 py-2">
      <div className="max-w-[80%]">
        {editing ? (
          <div className="flex flex-col gap-2">
            <textarea
              ref={taRef}
              value={draft}
              onChange={onInput}
              onKeyDown={onKey}
              rows={1}
              className="bg-surface/60 border border-accent/40 text-text rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] leading-relaxed resize-none outline-none focus:border-accent w-full min-w-[280px]"
              style={{ overflow: "hidden" }}
            />
            <div className="flex gap-2 justify-end">
              <button onClick={() => setEditing(false)} className="btn-ghost-sm">Cancel</button>
              <button onClick={save} className="px-3 py-1 text-xs btn-primary">Save</button>
            </div>
          </div>
        ) : (
          <>
            <div className="relative">
              <span className="absolute -top-2.5 right-3 text-[10px] font-semibold text-text-subtle bg-canvas border border-surface rounded-full px-1.5 py-0.5 leading-none">
                #{position}/{total}
              </span>
              <div className="bg-surface/60 border border-surface text-text-subtle rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] leading-relaxed whitespace-pre-wrap">
                {item.content}
              </div>
            </div>
            <div className="flex items-center justify-end gap-1 mt-1.5 opacity-0 group-hover/qi:opacity-100 transition-opacity">
              <button onClick={startEdit}    title="Edit queued message"  className="btn-icon"><Pencil size={13} /></button>
              <button onClick={handleRemove} title="Remove from queue"    className="btn-icon-danger"><Trash2 size={13} /></button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

// ── Chat ─────────────────────────────────────────────────────────────────────

export function Chat() {
  const messages      = useStore($messages);
  const activeSessionID = useStore($activeSessionID);
  const busySessions  = useStore($busySessions);
  const agentError    = useStore($agentError);
  const selectedIDs   = useStore($selectedMessageIDs);
  const messageQueue  = useStore($messageQueue);
  const sessions      = useStore($sessions);

  const bottomRef   = useRef<HTMLDivElement>(null);
  const scrollRef   = useRef<HTMLDivElement>(null);
  const isAtBottomRef = useRef(true);

  const activeSession = useMemo(
    () => sessions.find((s) => s.ID === activeSessionID) ?? null,
    [sessions, activeSessionID]
  );
  const todos        = useMemo(() => activeSession?.Todos ?? [], [activeSession]);
  const isBusy       = useMemo(() => activeSessionID ? busySessions.has(activeSessionID) : false, [activeSessionID, busySessions]);
  const selectionActive = selectedIDs.size > 0;
  const queuedItems  = useMemo(() => activeSessionID ? (messageQueue.get(activeSessionID) ?? []) : [], [activeSessionID, messageQueue]);

  const lastUserMsgID = useMemo(
    () => (isBusy ? null : [...messages].reverse().find((m) => m.Role === "user")?.ID ?? null),
    [messages, isBusy]
  );

  const forkDefaultTitle = useMemo(
    () => (activeSession?.Title || "Session") + " fork",
    [activeSession]
  );

  const [confirm, setConfirm] = useState<{ text: string; action: () => void } | null>(null);

  const handleScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    isAtBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight <= 80;
  }, []);

  const handleWheel = useCallback((e: React.WheelEvent<HTMLDivElement>) => {
    if (e.shiftKey) {
      const el = scrollRef.current;
      if (!el) return;
      e.preventDefault();
      // Scroll 5x faster when Shift is held
      el.scrollTop += e.deltaY * 5;
    }
  }, []);

  useEffect(() => {
    clearSelection();
    isAtBottomRef.current = true;
    bottomRef.current?.scrollIntoView({ behavior: "instant" });
  }, [activeSessionID]);

  useEffect(() => {
    if (isAtBottomRef.current) {
      bottomRef.current?.scrollIntoView({ behavior: "instant" });
    }
  }, [messages, isBusy, agentError]);

  const handleRangeSelect = useCallback((clickedIndex: number) => {
    const selected = selectedIDs;
    if (selected.size === 0) {
      toggleMessageSelection(messages[clickedIndex].ID);
      return;
    }
    let above = -1;
    let below = -1;
    for (let i = 0; i < messages.length; i++) {
      if (selected.has(messages[i].ID)) {
        if (i < clickedIndex) above = i;
        if (i > clickedIndex && below === -1) below = i;
      }
    }
    const from = above !== -1 ? above : clickedIndex;
    const to   = below !== -1 ? below : clickedIndex;
    const ids  = messages.slice(Math.min(from, clickedIndex), Math.max(to, clickedIndex) + 1).map(m => m.ID);
    selectMessageIDs(ids);
  }, [messages, selectedIDs]);

  const requestDeleteOne = useCallback((id: string) => {
    setConfirm({
      text: "Delete this message?",
      action: () => { deleteMessage(id); clearSelection(); },
    });
  }, []);

  const requestDeleteSelected = useCallback(() => {
    const ids = Array.from(selectedIDs);
    setConfirm({
      text: `Delete ${ids.length} selected message${ids.length === 1 ? "" : "s"}?`,
      action: () => { deleteMessages(ids); clearSelection(); },
    });
  }, [selectedIDs]);

  return (
    <div className="flex-1 flex flex-col overflow-hidden relative bg-canvas">
      <div ref={scrollRef} onScroll={handleScroll} onWheel={handleWheel} className="flex-1 overflow-y-auto overflow-x-hidden py-8 flex flex-col">
        {!activeSessionID ? (
          <div className="empty-state">
            <div className="empty-state-icon">
              <MessageSquare size={40} />
            </div>
            <p className="empty-state-title">No session selected</p>
            <p className="empty-state-desc">Select a session from the sidebar or create a new one</p>
          </div>
        ) : messages.length === 0 && !agentError ? (
          <div className="empty-state">
            <div className="empty-state-icon">
              <Sparkles size={40} />
            </div>
            <p className="empty-state-title">No messages yet</p>
            <p className="empty-state-desc">Say something to get started</p>
          </div>
        ) : (
          messages.map((m, i) => (
            <Message
              key={m.ID}
              index={i}
              message={m}
              onDeleteRequest={requestDeleteOne}
              onRangeSelect={handleRangeSelect}
              selectionActive={selectionActive}
              isLastUserMsg={m.ID === lastUserMsgID}
              isSelected={selectedIDs.has(m.ID)}
              forkDefaultTitle={forkDefaultTitle}
              sessionID={activeSessionID ?? ""}
            />
          ))
        )}

        {agentError && (
          <div className="px-5 py-2">
            <div className="chat-error-banner">
              <span className="text-red text-lg shrink-0 mt-0.5">⚠</span>
              <p className="text-[15px] text-red/80 leading-relaxed flex-1 break-words">{agentError}</p>
              <button onClick={() => $agentError.set(null)} aria-label="Dismiss" className="text-red/40 hover:text-red/70 transition-colors shrink-0 text-xl leading-none mt-0.5">
                <X size={16} />
              </button>
            </div>
          </div>
        )}

        {isBusy && (
          <div className="flex items-center gap-3 px-5 py-2">
            <div className="flex gap-1.5 animate-pulse-dots">
              <span className="w-2 h-2 rounded-full bg-accent inline-block" />
              <span className="w-2 h-2 rounded-full bg-accent inline-block" />
              <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            </div>
            <button
              onClick={() => activeSessionID && ws.send("cancel_agent", { sessionID: activeSessionID })}
              className="btn-stop"
            >
              <Square size={11} fill="currentColor" />
              Stop
            </button>
          </div>
        )}

        <div ref={bottomRef} className="h-8 shrink-0" />
      </div>

      {queuedItems.length > 0 && activeSessionID && (
        <div className="shrink-0 border-t border-surface bg-canvas">
          <div className="flex items-center gap-2 px-5 py-1.5">
            <div className="divider-line" />
            <span className="section-label">Queue · {queuedItems.length}</span>
            <div className="divider-line" />
          </div>
          {queuedItems.map((item, idx) => (
            <QueuedMessageItem key={item.id} item={item} sessionID={activeSessionID} position={idx + 1} total={queuedItems.length} />
          ))}
        </div>
      )}

      {selectionActive && (
        <div className="selection-toolbar">
          <span className="text-sm text-text-subtle">{selectedIDs.size} selected</span>
          <button onClick={requestDeleteSelected} className="btn-delete">
            <Trash2 size={14} />
            Delete selected
          </button>
          <button onClick={clearSelection} className="ml-auto flex items-center gap-1.5 text-sm text-text-subtle hover:text-text transition-colors">
            <X size={14} />
            Cancel
          </button>
        </div>
      )}

      {activeSessionID && <TodoList sessionID={activeSessionID} todos={todos} />}

      <ChatToolbar />
      <PermissionDialog />
      <ChatInput />

      {confirm && (
        <ConfirmDialog
          title="Delete message"
          message={confirm.text}
          confirmLabel="Delete"
          onConfirm={() => { confirm.action(); setConfirm(null); }}
          onCancel={() => setConfirm(null)}
        />
      )}
    </div>
  );
}
