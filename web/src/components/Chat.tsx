import { useEffect, useRef, useState } from "react";
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
  deleteMessage,
  deleteMessages,
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

  function startEdit() {
    setDraft(item.content);
    setEditing(true);
  }

  useEffect(() => {
    if (editing && taRef.current) {
      taRef.current.focus();
      taRef.current.selectionStart = taRef.current.value.length;
      taRef.current.style.height = "auto";
      taRef.current.style.height = taRef.current.scrollHeight + "px";
    }
  }, [editing]);

  function save() {
    const trimmed = draft.trim();
    if (trimmed) updateQueuedMessage(sessionID, item.id, trimmed);
    setEditing(false);
  }

  function onKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Escape") setEditing(false);
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) save();
  }

  function onInput(e: React.ChangeEvent<HTMLTextAreaElement>) {
    setDraft(e.target.value);
    e.target.style.height = "auto";
    e.target.style.height = e.target.scrollHeight + "px";
  }

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
              <button
                onClick={() => setEditing(false)}
                className="px-3 py-1 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
              >
                Cancel
              </button>
              <button
                onClick={save}
                className="px-3 py-1 text-xs bg-accent-fill text-white/90 rounded-lg hover:opacity-90 transition-opacity"
              >
                Save
              </button>
            </div>
          </div>
        ) : (
          <>
            <div className="relative">
              {/* position badge */}
              <span className="absolute -top-2.5 right-3 text-[10px] font-semibold text-text-subtle bg-canvas border border-surface rounded-full px-1.5 py-0.5 leading-none">
                #{position}/{total}
              </span>
              <div className="bg-surface/60 border border-surface text-text-subtle rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] leading-relaxed whitespace-pre-wrap">
                {item.content}
              </div>
            </div>
            <div className="flex items-center justify-end gap-1 mt-1.5 opacity-0 group-hover/qi:opacity-100 transition-opacity">
              <button
                onClick={startEdit}
                title="Edit queued message"
                className="p-1.5 text-text-subtle hover:text-accent transition-colors rounded"
              >
                <Pencil size={13} />
              </button>
              <button
                onClick={() => removeQueuedMessage(sessionID, item.id)}
                title="Remove from queue"
                className="p-1.5 text-text-subtle hover:text-red transition-colors rounded"
              >
                <Trash2 size={13} />
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

// ── Chat ─────────────────────────────────────────────────────────────────────

export function Chat() {
  const messages = useStore($messages);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const agentError = useStore($agentError);
  const selectedIDs = useStore($selectedMessageIDs);
  const messageQueue = useStore($messageQueue);
  const bottomRef = useRef<HTMLDivElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const isAtBottomRef = useRef(true);

  const sessions = useStore($sessions);
  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;
  const todos = activeSession?.Todos ?? [];

  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const selectionActive = selectedIDs.size > 0;
  const queuedItems = activeSessionID ? (messageQueue.get(activeSessionID) ?? []) : [];

  // Last user message — Rerun button shown only here
  const lastUserMsgID = [...messages].reverse().find((m) => m.Role === "user")?.ID ?? null;

  const [confirm, setConfirm] = useState<{ text: string; action: () => void } | null>(null);

  function handleScroll() {
    const el = scrollRef.current;
    if (!el) return;
    isAtBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight <= 80;
  }

  useEffect(() => {
    clearSelection();
    isAtBottomRef.current = true;
    bottomRef.current?.scrollIntoView({ behavior: "instant" });
  }, [activeSessionID]);

  useEffect(() => {
    if (isAtBottomRef.current) {
      bottomRef.current?.scrollIntoView({ behavior: "smooth" });
    }
  }, [messages, isBusy, agentError]);

  function requestDeleteOne(id: string) {
    setConfirm({
      text: "Delete this message?",
      action: () => { deleteMessage(id); clearSelection(); },
    });
  }

  function requestDeleteSelected() {
    const ids = Array.from(selectedIDs);
    setConfirm({
      text: `Delete ${ids.length} selected message${ids.length === 1 ? "" : "s"}?`,
      action: () => { deleteMessages(ids); clearSelection(); },
    });
  }

  return (
    <div className="flex-1 flex flex-col overflow-hidden relative bg-canvas">
      <div ref={scrollRef} onScroll={handleScroll} className="flex-1 overflow-y-auto py-8 flex flex-col">
        {!activeSessionID ? (
          <div className="flex flex-col items-center justify-center flex-1 text-center px-8">
            <div className="w-20 h-20 rounded-3xl bg-base-overlay flex items-center justify-center mb-6 text-text-subtle">
              <MessageSquare size={40} />
            </div>
            <p className="text-xl font-bold text-text-muted">No session selected</p>
            <p className="text-text-subtle text-base mt-2">Select a session from the sidebar or create a new one</p>
          </div>
        ) : messages.length === 0 && !agentError ? (
          <div className="flex flex-col items-center justify-center flex-1 text-center px-8">
            <div className="w-20 h-20 rounded-3xl bg-base-overlay flex items-center justify-center mb-6 text-text-subtle">
              <Sparkles size={40} />
            </div>
            <p className="text-xl font-bold text-text-muted">No messages yet</p>
            <p className="text-text-subtle text-base mt-2">Say something to get started</p>
          </div>
        ) : (
          messages.map((m) => (
            <Message
              key={m.ID}
              message={m}
              onDeleteRequest={requestDeleteOne}
              selectionActive={selectionActive}
              isLastUserMsg={m.ID === lastUserMsgID && !isBusy}
            />
          ))
        )}

        {agentError && (
          <div className="px-10 py-2">
            <div className="flex items-start gap-3 bg-red/5 border border-red/20 rounded-2xl px-5 py-4">
              <span className="text-red text-lg shrink-0 mt-0.5">⚠</span>
              <p className="text-[15px] text-red/80 leading-relaxed flex-1 break-words">{agentError}</p>
              <button
                onClick={() => $agentError.set(null)}
                aria-label="Dismiss"
                className="text-red/40 hover:text-red/70 transition-colors shrink-0 text-xl leading-none mt-0.5"
              >
                <X size={16} />
              </button>
            </div>
          </div>
        )}

        {/* Typing indicator + Stop button */}
        {isBusy && (
          <div className="flex items-center gap-4 px-10 py-5">
            <div className="flex gap-2 animate-pulse-dots">
              <span className="w-2.5 h-2.5 rounded-full bg-accent/60 inline-block" />
              <span className="w-2.5 h-2.5 rounded-full bg-accent/60 inline-block" />
              <span className="w-2.5 h-2.5 rounded-full bg-accent/60 inline-block" />
            </div>
            <button
              onClick={() => activeSessionID && ws.send("cancel_agent", { sessionID: activeSessionID })}
              className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-semibold bg-red/10 text-red border border-red/20 rounded-lg hover:bg-red/20 transition-colors"
            >
              <Square size={11} fill="currentColor" />
              Stop
            </button>
          </div>
        )}

        <div ref={bottomRef} className="h-8 shrink-0" />
      </div>

      {/* Queued messages — outside scroll area so streaming content doesn't cause jitter */}
      {queuedItems.length > 0 && activeSessionID && (
        <div className="shrink-0 border-t border-surface bg-canvas">
          <div className="flex items-center gap-2 px-10 py-1.5">
            <div className="flex-1 h-px bg-surface/50" />
            <span className="text-[11px] text-text-subtle font-medium uppercase tracking-wider">
              Queue · {queuedItems.length}
            </span>
            <div className="flex-1 h-px bg-surface/50" />
          </div>
          {queuedItems.map((item, idx) => (
            <QueuedMessageItem
              key={item.id}
              item={item}
              sessionID={activeSessionID}
              position={idx + 1}
              total={queuedItems.length}
            />
          ))}
        </div>
      )}

      {/* Batch selection toolbar */}
      {selectionActive && (
        <div className="border-t border-surface bg-base-overlay px-6 py-3 flex items-center gap-4 shrink-0">
          <span className="text-sm text-text-subtle">{selectedIDs.size} selected</span>
          <button
            onClick={requestDeleteSelected}
            className="flex items-center gap-1.5 px-3 py-1.5 text-sm bg-red/10 text-red border border-red/20 rounded-lg hover:bg-red/20 transition-colors font-medium"
          >
            <Trash2 size={14} />
            Delete selected
          </button>
          <button
            onClick={clearSelection}
            className="ml-auto flex items-center gap-1.5 text-sm text-text-subtle hover:text-text transition-colors"
          >
            <X size={14} />
            Cancel
          </button>
        </div>
      )}

      {activeSessionID && todos.length > 0 && (
        <TodoList sessionID={activeSessionID} todos={todos} />
      )}

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
