import { useEffect, useRef, useState } from "react";
import { useStore } from "@nanostores/react";
import {
  $messages,
  $activeSessionID,
  $busySessions,
  $agentError,
  $selectedMessageIDs,
  clearSelection,
  deleteMessage,
  deleteMessages,
} from "../store";
import { Message } from "./Message";
import { ChatInput } from "./ChatInput";
import { PermissionDialog } from "./PermissionDialog";
import { MessageSquare, Sparkles, Trash2, X } from "lucide-react";

// ── Confirmation dialog ───────────────────────────────────────────────────────

function ConfirmDialog({
  message,
  onConfirm,
  onCancel,
}: {
  message: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  // Close on Escape
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onCancel();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onCancel]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={onCancel}
    >
      <div
        className="bg-white border border-surface rounded-2xl shadow-xl px-8 py-6 max-w-sm w-full mx-4"
        onClick={(e) => e.stopPropagation()}
      >
        <p className="text-text text-[15px] leading-relaxed mb-6">{message}</p>
        <div className="flex gap-3 justify-end">
          <button
            onClick={onCancel}
            className="px-4 py-2 text-sm text-text-subtle hover:text-text transition-colors rounded-xl hover:bg-base-overlay"
          >
            Cancel
          </button>
          <button
            onClick={onConfirm}
            className="px-4 py-2 text-sm bg-red text-white rounded-xl hover:opacity-90 transition-opacity font-medium"
          >
            Delete
          </button>
        </div>
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
  const bottomRef = useRef<HTMLDivElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  // true = user is at (or near) the bottom → auto-scroll is active
  const isAtBottomRef = useRef(true);

  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const selectionActive = selectedIDs.size > 0;

  // Confirmation state: null = no dialog, otherwise the confirmed action
  const [confirm, setConfirm] = useState<{ text: string; action: () => void } | null>(null);

  // Track whether the user is near the bottom (within 80px).
  function handleScroll() {
    const el = scrollRef.current;
    if (!el) return;
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    isAtBottomRef.current = distanceFromBottom <= 80;
  }

  // Clear selection when active session changes and jump straight to bottom.
  useEffect(() => {
    clearSelection();
    isAtBottomRef.current = true;
    // Instant jump on session switch (no animation so we don't see a flash).
    bottomRef.current?.scrollIntoView({ behavior: "instant" });
  }, [activeSessionID]);

  // Auto-scroll only when the user is already at the bottom.
  useEffect(() => {
    if (isAtBottomRef.current) {
      bottomRef.current?.scrollIntoView({ behavior: "smooth" });
    }
  }, [messages, isBusy, agentError]);

  function requestDeleteOne(id: string) {
    setConfirm({
      text: "Delete this message?",
      action: () => {
        deleteMessage(id);
        clearSelection();
      },
    });
  }

  function requestDeleteSelected() {
    const ids = Array.from(selectedIDs);
    setConfirm({
      text: `Delete ${ids.length} selected message${ids.length === 1 ? "" : "s"}?`,
      action: () => {
        deleteMessages(ids);
        clearSelection();
      },
    });
  }

  return (
    <div className="flex-1 flex flex-col overflow-hidden relative bg-white">
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
                className="text-red/40 hover:text-red/70 transition-colors shrink-0 text-xl leading-none mt-0.5"
              >
                ×
              </button>
            </div>
          </div>
        )}

        {isBusy && (
          <div className="flex gap-2 px-10 py-6 animate-pulse-dots">
            <span className="w-3 h-3 rounded-full bg-accent/60 inline-block" />
            <span className="w-3 h-3 rounded-full bg-accent/60 inline-block" />
            <span className="w-3 h-3 rounded-full bg-accent/60 inline-block" />
          </div>
        )}
        <div ref={bottomRef} className="h-8 shrink-0" />
      </div>

      {/* Batch selection toolbar */}
      {selectionActive && (
        <div className="border-t border-surface bg-base-overlay px-6 py-3 flex items-center gap-4 shrink-0">
          <span className="text-sm text-text-subtle">
            {selectedIDs.size} selected
          </span>
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

      <PermissionDialog />
      <ChatInput />

      {confirm && (
        <ConfirmDialog
          message={confirm.text}
          onConfirm={() => {
            confirm.action();
            setConfirm(null);
          }}
          onCancel={() => setConfirm(null)}
        />
      )}
    </div>
  );
}
