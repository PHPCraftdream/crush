import { useState, useRef, useCallback, useEffect } from "react";
import { useStore } from "@nanostores/react";
import {
  $activeSessionID,
  $busySessions,
  enqueueMessage,
  sendWithSmallModel,
} from "../store";
import { ws } from "../ws";
import { ListOrdered, Send, SendHorizonal, Paperclip, X } from "lucide-react";

interface PendingAttachment {
  fileName: string;
  mimeType: string;
  data: string; // base64
  size: number;
}

async function readFileAsBase64(file: File): Promise<PendingAttachment> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const arrayBuffer = reader.result as ArrayBuffer;
      const bytes = new Uint8Array(arrayBuffer);
      let binary = "";
      bytes.forEach((b) => (binary += String.fromCharCode(b)));
      resolve({
        fileName: file.name,
        mimeType: file.type || "application/octet-stream",
        data: btoa(binary),
        size: file.size,
      });
    };
    reader.onerror = reject;
    reader.readAsArrayBuffer(file);
  });
}

function formatBytes(n: number): string {
  if (n >= 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + " MB";
  if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
  return n + " B";
}

export function ChatInput() {
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<PendingAttachment[]>([]);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (activeSessionID && textareaRef.current) {
      textareaRef.current.focus();
    }
  }, [activeSessionID]);

  const agentBusy = activeSessionID ? busySessions.has(activeSessionID) : false;

  const send = useCallback(() => {
    const msg = text.trim();
    if (!msg || !activeSessionID) return;
    if (agentBusy) {
      enqueueMessage(activeSessionID, msg);
    } else {
      const payload: Record<string, unknown> = { sessionID: activeSessionID, content: msg };
      if (attachments.length > 0) {
        payload.attachments = attachments.map((a) => ({
          fileName: a.fileName,
          mimeType: a.mimeType,
          data: a.data,
        }));
      }
      ws.send("send_message", payload);
      setAttachments([]);
    }
    setText("");
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy, attachments]);

  const sendSmall = useCallback(() => {
    const msg = text.trim();
    if (!msg || !activeSessionID || agentBusy) return;
    sendWithSmallModel(activeSessionID, msg);
    setText("");
    setAttachments([]);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy]);

  function onKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  }

  function onInput(e: React.ChangeEvent<HTMLTextAreaElement>) {
    setText(e.target.value);
    const el = e.target;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, 240) + "px";
  }

  async function onFileChange(e: React.ChangeEvent<HTMLInputElement>) {
    const files = Array.from(e.target.files ?? []);
    if (files.length === 0) return;
    const reads = await Promise.all(files.map(readFileAsBase64));
    setAttachments((prev) => [...prev, ...reads]);
    // Reset so same file can be re-added
    if (fileInputRef.current) fileInputRef.current.value = "";
  }

  function removeAttachment(idx: number) {
    setAttachments((prev) => prev.filter((_, i) => i !== idx));
  }

  // Drag-and-drop support
  function onDragOver(e: React.DragEvent) {
    e.preventDefault();
  }

  async function onDrop(e: React.DragEvent) {
    e.preventDefault();
    const files = Array.from(e.dataTransfer.files);
    if (files.length === 0) return;
    const reads = await Promise.all(files.map(readFileAsBase64));
    setAttachments((prev) => [...prev, ...reads]);
  }

  const canSend = !!text.trim() && !!activeSessionID;
  const placeholder = activeSessionID
    ? agentBusy
      ? "Message… (will be queued)"
      : "Message… (Enter to send)"
    : "Select or create a session";

  return (
    <div className="px-8 pt-2 pb-6 bg-canvas shrink-0">
      {/* Attachment badges */}
      {attachments.length > 0 && (
        <div className="flex flex-wrap gap-2 mb-2">
          {attachments.map((att, idx) => (
            <div
              key={idx}
              className="flex items-center gap-1.5 bg-base-overlay border border-surface rounded-lg px-2.5 py-1 text-xs text-text"
            >
              <Paperclip size={11} className="text-text-subtle shrink-0" />
              <span className="truncate max-w-[140px]">{att.fileName}</span>
              <span className="text-text-muted shrink-0">({formatBytes(att.size)})</span>
              <button
                onClick={() => removeAttachment(idx)}
                className="text-text-subtle hover:text-text transition-colors ml-0.5"
                title="Remove attachment"
              >
                <X size={11} />
              </button>
            </div>
          ))}
        </div>
      )}

      <div
        className={`flex gap-4 items-end bg-base-overlay border rounded-2xl px-5 py-4 transition-all ${
          activeSessionID
            ? "border-surface focus-within:border-accent/50 focus-within:shadow-md focus-within:bg-canvas"
            : "border-surface opacity-60"
        }`}
        onDragOver={onDragOver}
        onDrop={onDrop}
      >
        <textarea
          ref={textareaRef}
          value={text}
          onChange={onInput}
          onKeyDown={onKey}
          placeholder={placeholder}
          disabled={!activeSessionID}
          autoFocus
          rows={1}
          className="flex-1 bg-transparent border-none outline-none resize-none text-text text-[16px] leading-relaxed min-h-[28px] max-h-80 overflow-y-auto disabled:cursor-not-allowed placeholder:text-text-subtle font-medium"
        />
        <div className="flex items-center gap-2 shrink-0">
          {/* File attach */}
          <input
            ref={fileInputRef}
            type="file"
            multiple
            className="hidden"
            onChange={onFileChange}
            aria-label="Attach files"
          />
          {!agentBusy && (
            <button
              onClick={() => fileInputRef.current?.click()}
              disabled={!activeSessionID}
              title="Attach files"
              className="p-2.5 text-text-subtle hover:text-accent hover:bg-accent/10 rounded-xl transition-all active:scale-95 disabled:opacity-30"
            >
              <Paperclip size={18} />
            </button>
          )}
          {!agentBusy && (
            <button
              onClick={sendSmall}
              disabled={!canSend}
              title="Send with lightweight model"
              className="p-2.5 text-text-subtle hover:text-accent hover:bg-accent/10 rounded-xl transition-all active:scale-95 disabled:opacity-30"
            >
              <SendHorizonal size={18} />
            </button>
          )}
          <button
            onClick={send}
            disabled={!canSend}
            className={`font-bold rounded-xl px-5 py-2.5 text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2 ${
              agentBusy
                ? "bg-base-overlay border border-surface text-text-subtle hover:border-accent/50 hover:text-text"
                : "bg-accent-fill text-white/90 hover:opacity-90"
            }`}
          >
            {agentBusy ? (
              <>
                <ListOrdered size={15} />
                Queue
              </>
            ) : (
              <>
                <Send size={15} />
                Send
              </>
            )}
          </button>
        </div>
      </div>
      <p className="text-center text-text-subtle text-sm mt-3 font-medium">
        Shift+Enter for newline · Drop files to attach
      </p>
    </div>
  );
}
