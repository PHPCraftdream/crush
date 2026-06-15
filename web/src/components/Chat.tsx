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
import { Message, ToolActivityGroup, StandaloneThinking } from "./Message";
import { ChatInput } from "./ChatInput";
import { PermissionDialog } from "./PermissionDialog";
import { ConfirmDialog } from "./ConfirmDialog";
import { ChatToolbar } from "./ChatToolbar";
import { TodoList } from "./TodoList";
import { MessageSquare, Pencil, Sparkles, Square, Trash2, X } from "lucide-react";
import type { Message as Msg, ContentPart } from "../types";

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

// ── Cross-message tool-run grouping ──────────────────────────────────────────
//
// The agent emits a brand-new assistant message per turn-step, and almost
// every step carries a `thinking` part next to its tool_call (the model's
// pre-action reasoning). The previous version of this grouper rejected any
// message that had thinking — so each "thinking + tool_call + tool_result"
// step landed as its own standalone <Message> with a near-empty "1 action"
// accordion, defeating the whole point of grouping.
//
// The new rule lifts grouping to the part level. We walk through the parts
// of consecutive assistant messages and pick out a single contiguous tool
// burst: a stretch of tool_call / tool_result parts across N adjacent
// assistant messages, with intervening thinking parts pulled out as their
// own standalone items. A `text` part, a user message, an error/canceled
// finish, a summary or hidden message all flush the burst — so the
// accordion always corresponds to one uninterrupted stretch of tool work.
//
// Inside the burst, ToolActivityGroup pairs calls with their results by
// ToolCallID even though they came from different message rows, and the
// "last row open, prior closed, user-clicks pin" auto-rule kicks in for
// real.

interface PartLike { type: string; Reason?: string; Thinking?: string }

type RenderItem =
  | { kind: "message"; message: Msg; index: number }
  | { kind: "thinking"; messageID: string; partIndex: number; thinking: string; done: boolean }
  | { kind: "toolrun"; parts: ContentPart[]; firstMsgID: string };

function buildRenderItems(messages: Msg[]): RenderItem[] {
  const out: RenderItem[] = [];
  let burstParts: ContentPart[] = [];
  let burstFirstID = "";

  const flushBurst = () => {
    if (burstParts.length === 0) return;
    out.push({ kind: "toolrun", parts: burstParts, firstMsgID: burstFirstID });
    burstParts = [];
    burstFirstID = "";
  };

  messages.forEach((m, i) => {
    // Anything that is not a plain assistant message flushes the burst and
    // renders as its own <Message> — user prompts, summary cards, hidden
    // rows (Message returns null for those, but we still want the run cut).
    if (m.Role !== "assistant" || m.Hidden || m.IsSummaryMessage) {
      flushBurst();
      out.push({ kind: "message", message: m, index: i });
      return;
    }

    // Scan the message's parts. A `text` or error finish forces a flush AND
    // sends the whole message through <Message> so its body renders inline
    // (we don't try to tease apart text + tool inside the same message —
    // that's rare and not worth the complexity). Otherwise, thinking parts
    // become standalone items in the burst's region, and tool parts append
    // to the running burst.
    let hasTool = false;
    let hasText = false;
    let hasErrorFinish = false;
    for (const raw of m.Parts) {
      const p = raw as unknown as PartLike;
      if (p.type === "text") hasText = true;
      if (p.type === "tool_call" || p.type === "tool_result") hasTool = true;
      if (p.type === "finish" && (p.Reason === "error" || p.Reason === "canceled")) hasErrorFinish = true;
    }

    if (hasText || hasErrorFinish || !hasTool) {
      // Text-bearing, errored, or pure-thinking-without-tools message:
      // flush the burst (this message ends any prior tool stretch) and
      // render the message in full.
      flushBurst();
      out.push({ kind: "message", message: m, index: i });
      return;
    }

    // Tool-bearing message with no text and no error finish — fold it.
    if (burstFirstID === "") burstFirstID = m.ID;
    m.Parts.forEach((p, partIdx) => {
      if (p.type === "thinking") {
        const thinking = (p as unknown as PartLike).Thinking ?? "";
        out.push({ kind: "thinking", messageID: m.ID, partIndex: partIdx, thinking, done: true });
      } else if (p.type === "tool_call" || p.type === "tool_result") {
        burstParts.push(p);
      }
      // finish (non-error) and other parts are dropped — they have no
      // visible effect inside a tool burst.
    });
  });

  flushBurst();
  return out;
}

function ToolRun({ parts, firstMsgID, sessionID, isLive }: { parts: ContentPart[]; firstMsgID: string; sessionID: string; isLive: boolean }) {
  // ToolActivityGroup pairs call↔result by ToolCallID — no further prep
  // needed here, just give each part a stable index for its key.
  const items = useMemo(
    () => parts.map((part, idx) => ({ part, idx })),
    [parts]
  );
  return (
    <div
      id={firstMsgID ? `msg-${firstMsgID}` : undefined}
      data-msg-role="assistant"
      data-tool-run="true"
      className="msg-row flex flex-col px-5 py-3"
      title={`${parts.length} tool parts grouped across messages`}
    >
      <div className="w-full min-w-0">
        <ToolActivityGroup items={items} live={isLive} />
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

  // Group consecutive tool-only assistant messages into a single ToolRun so
  // a long burst of N steps renders as one container with N actions instead
  // of N near-empty per-message containers.
  const renderItems = useMemo(() => buildRenderItems(messages), [messages]);

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
          renderItems.map((item, ri) => {
            if (item.kind === "toolrun") {
              return (
                <ToolRun
                  key={`run-${item.firstMsgID}-${ri}`}
                  parts={item.parts}
                  firstMsgID={item.firstMsgID}
                  sessionID={activeSessionID ?? ""}
                  isLive={isBusy}
                />
              );
            }
            if (item.kind === "thinking") {
              return (
                <StandaloneThinking
                  key={`th-${item.messageID}-${item.partIndex}`}
                  messageID={item.messageID}
                  partIndex={item.partIndex}
                  thinking={item.thinking}
                  done={item.done}
                />
              );
            }
            const m = item.message;
            return (
              <Message
                key={m.ID}
                index={item.index}
                message={m}
                onDeleteRequest={requestDeleteOne}
                onRangeSelect={handleRangeSelect}
                selectionActive={selectionActive}
                isLastUserMsg={m.ID === lastUserMsgID}
                isSelected={selectedIDs.has(m.ID)}
                forkDefaultTitle={forkDefaultTitle}
                sessionID={activeSessionID ?? ""}
              />
            );
          })
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
