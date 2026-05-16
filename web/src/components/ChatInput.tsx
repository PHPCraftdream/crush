import { useState, useRef, useCallback, useEffect, useMemo } from "react";
import { useStore } from "@nanostores/react";
import {
  $activeSessionID,
  $busySessions,
  $skills,
  $lastUsedSkill,
  enqueueMessage,
  dequeueAllMessages,
  sendWithSmallModel,
  setLastUsedSkill,
} from "../store";
import { ws } from "../ws";
import { ListOrdered, Send, SendHorizonal, Paperclip, X, Zap } from "lucide-react";
import type { SkillInfo } from "../types";

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

// Source badge colors
const SOURCE_COLORS: Record<string, string> = {
  claude: "bg-[#d97706]/15 text-[#d97706]",
  gemini: "bg-[#1a73e8]/15 text-[#1a73e8]",
  qwen: "bg-[#7c3aed]/15 text-[#7c3aed]",
  cursor: "bg-[#0ea5e9]/15 text-[#0ea5e9]",
  zed: "bg-[#059669]/15 text-[#059669]",
  windsurf: "bg-[#0891b2]/15 text-[#0891b2]",
  crush: "bg-accent/15 text-accent",
  local: "bg-surface text-text-muted",
};

function sourceBadgeClass(source?: string): string {
  return SOURCE_COLORS[source ?? ""] ?? SOURCE_COLORS.local;
}

interface SlashMenuProps {
  items: SkillInfo[];
  selectedIdx: number;
  onSelect: (skill: SkillInfo, sendNow: boolean) => void;
}

function SlashMenu({ items, selectedIdx, onSelect }: SlashMenuProps) {
  const selectedRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    selectedRef.current?.scrollIntoView({ block: "nearest" });
  }, [selectedIdx]);

  if (items.length === 0) return null;

  return (
    <div className="absolute bottom-full left-0 right-0 mb-2 bg-base-overlay border border-surface rounded-2xl shadow-2xl overflow-hidden z-50">
      <div className="max-h-72 overflow-y-auto">
        {items.map((skill, i) => (
          <button
            key={`${skill.source}-${skill.name}`}
            ref={i === selectedIdx ? selectedRef : null}
            onMouseDown={(e) => {
              e.preventDefault(); // keep focus on textarea
              onSelect(skill, false);
            }}
            className={`w-full text-left px-4 py-2.5 flex items-center gap-3 transition-colors border-b border-surface/50 last:border-0 ${
              i === selectedIdx
                ? "bg-accent/20 border-l-2 border-l-accent pl-[14px]"
                : "hover:bg-base-subtle border-l-2 border-l-transparent pl-[14px]"
            }`}
          >
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2">
                <span className={`font-semibold text-sm ${i === selectedIdx ? "text-accent" : "text-text"}`}>/{skill.name}</span>
                {skill.source && (
                  <span className={`text-[10px] px-1.5 py-0.5 rounded font-mono font-medium ${sourceBadgeClass(skill.source)}`}>
                    {skill.source}
                  </span>
                )}
              </div>
              <p className="text-text-muted text-xs truncate mt-0.5 leading-relaxed">
                {skill.description}
              </p>
            </div>
          </button>
        ))}
      </div>
      <div className="px-4 py-1.5 border-t border-surface/50 flex items-center gap-3 text-[11px] text-text-subtle">
        <span><kbd className="font-mono">↑↓</kbd> navigate</span>
        <span><kbd className="font-mono">Tab</kbd> fill</span>
        <span><kbd className="font-mono">Enter</kbd> send</span>
        <span><kbd className="font-mono">Esc</kbd> close</span>
      </div>
    </div>
  );
}

export function ChatInput() {
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<PendingAttachment[]>([]);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const skills = useStore($skills);
  const lastUsedSkill = useStore($lastUsedSkill);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // ── Slash menu state ──────────────────────────────────────────────────────
  const [slashOpen, setSlashOpen] = useState(false);
  const [slashIdx, setSlashIdx] = useState(0);

  // Text matches "/<word>" with no spaces — menu active
  const slashMatch = text.match(/^\/(\S*)$/);
  const slashQuery = slashMatch ? slashMatch[1] : null;

  const filteredSkills = useMemo(() => {
    if (slashQuery === null) return [];
    const q = slashQuery.toLowerCase();
    return skills.filter(
      (s) =>
        s.name.toLowerCase().includes(q) ||
        s.description.toLowerCase().includes(q)
    );
  }, [skills, slashQuery]);

  // Auto-open and pre-select
  useEffect(() => {
    if (slashQuery !== null && filteredSkills.length > 0) {
      setSlashOpen(true);
      // Pre-select last used if in results, otherwise best name match, otherwise first
      const lastIdx = filteredSkills.findIndex((s) => s.name === lastUsedSkill);
      if (lastIdx >= 0) {
        setSlashIdx(lastIdx);
      } else {
        // Exact prefix match ranks first
        const q = slashQuery.toLowerCase();
        const exactIdx = filteredSkills.findIndex((s) => s.name.toLowerCase().startsWith(q));
        setSlashIdx(exactIdx >= 0 ? exactIdx : 0);
      }
    } else {
      setSlashOpen(false);
    }
  }, [slashQuery, filteredSkills.length, lastUsedSkill]); // eslint-disable-line react-hooks/exhaustive-deps

  function handleSkillSelect(skill: SkillInfo, sendNow: boolean) {
    setLastUsedSkill(skill.name);
    setSlashOpen(false);
    if (sendNow) {
      if (!activeSessionID) return;
      const content = skill.instructions || `/${skill.name}`;
      ws.send("send_message", { sessionID: activeSessionID, content });
      setText("");
      if (textareaRef.current) textareaRef.current.style.height = "auto";
    } else {
      // Tab: fill in the command name + space so user can keep typing
      setText(`/${skill.name} `);
      setTimeout(() => {
        if (textareaRef.current) {
          textareaRef.current.focus();
          const len = textareaRef.current.value.length;
          textareaRef.current.setSelectionRange(len, len);
        }
      }, 0);
    }
  }

  // ─────────────────────────────────────────────────────────────────────────

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

  // Interrupt the in-flight turn AND submit immediately. The server's
  // interrupt_and_send handler queues the new message and cancels the
  // running turn; agent.Run()'s cancel-handling branch drains the queue
  // and re-enters Run() with this message — so the partial assistant
  // output stays in history and the new instruction picks up right away.
  // We also fold anything sitting in the local front-end queue into the
  // submitted text, otherwise those would race the cancel and arrive in
  // a separate send_message after the interrupt-turn finishes.
  const interrupt = useCallback(() => {
    if (!activeSessionID) return;
    const queued = dequeueAllMessages(activeSessionID);
    const parts: string[] = [];
    if (queued) parts.push(queued);
    const msg = text.trim();
    if (msg) parts.push(msg);
    const content = parts.join("\n\n");
    if (!content) return;
    const payload: Record<string, unknown> = { sessionID: activeSessionID, content };
    if (attachments.length > 0) {
      payload.attachments = attachments.map((a) => ({
        fileName: a.fileName,
        mimeType: a.mimeType,
        data: a.data,
      }));
    }
    ws.send("interrupt_and_send", payload);
    setText("");
    setAttachments([]);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, attachments]);

  function onKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (slashOpen && filteredSkills.length > 0) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setSlashIdx((i) => Math.min(i + 1, filteredSkills.length - 1));
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setSlashIdx((i) => Math.max(i - 1, 0));
        return;
      }
      if (e.key === "Tab") {
        e.preventDefault();
        handleSkillSelect(filteredSkills[slashIdx], false);
        return;
      }
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        handleSkillSelect(filteredSkills[slashIdx], true);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setSlashOpen(false);
        return;
      }
    }

    if (e.key === "Enter" && !e.shiftKey && !e.ctrlKey) {
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
    if (fileInputRef.current) fileInputRef.current.value = "";
  }

  function removeAttachment(idx: number) {
    setAttachments((prev) => prev.filter((_, i) => i !== idx));
  }

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

  async function onPaste(e: React.ClipboardEvent) {
    const items = Array.from(e.clipboardData.items);
    const pastedFiles: File[] = [];
    for (const item of items) {
      if (item.kind === "file") {
        const file = item.getAsFile();
        if (file) pastedFiles.push(file);
      }
    }
    if (pastedFiles.length === 0) return;
    e.preventDefault();
    const reads = await Promise.all(pastedFiles.map(readFileAsBase64));
    setAttachments((prev) => [...prev, ...reads]);
  }

  const canSend = !!text.trim() && !!activeSessionID;
  const placeholder = activeSessionID
    ? agentBusy
      ? "Message… (will be queued)"
      : "Message… (/ for skills, Enter to send)"
    : "Select or create a session";

  return (
    <div className="px-5 pt-2 pb-4 bg-canvas shrink-0">
      {/* Attachment badges */}
      {attachments.length > 0 && (
        <div className="flex flex-wrap gap-2 mb-2">
          {attachments.map((att, idx) => (
            <div
              key={idx}
              className="attachment-badge"
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

      <div className="relative">
        {/* Slash command dropdown — opens above input */}
        {slashOpen && (
          <SlashMenu
            items={filteredSkills}
            selectedIdx={slashIdx}
            onSelect={handleSkillSelect}
          />
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
            onPaste={onPaste}
            placeholder={placeholder}
            disabled={!activeSessionID}
            autoFocus
            rows={1}
            data-test-id="chat-input-textarea"
            className="flex-1 bg-transparent border-none outline-none resize-none text-text leading-relaxed min-h-[28px] max-h-80 overflow-y-auto disabled:cursor-not-allowed placeholder:text-text-subtle font-medium" style={{ fontSize: "var(--chat-font-size)" }}
          />
          <div className="flex items-center gap-2 shrink-0">
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
                className="btn-input-action"
              >
                <Paperclip size={18} />
              </button>
            )}
            {!agentBusy && (
              <button
                onClick={sendSmall}
                disabled={!canSend}
                title="Send with lightweight model"
                className="btn-input-action"
              >
                <SendHorizonal size={18} />
              </button>
            )}
            {agentBusy && (
              <button
                onClick={interrupt}
                disabled={!canSend}
                data-test-id="chat-input-interrupt-button"
                title="Cancel the current turn and submit this message immediately. The partial assistant reply stays in history."
                className="font-bold rounded-xl px-4 py-2.5 text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2 bg-yellow/15 border border-yellow/40 text-yellow hover:bg-yellow/25"
              >
                <Zap size={15} />
                Interrupt
              </button>
            )}
            <button
              onClick={send}
              disabled={!canSend}
              data-test-id="chat-input-send-button"
              className={`font-bold rounded-xl px-5 py-2.5 text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2 ${
                agentBusy
                  ? "bg-base-overlay border border-surface text-text-subtle hover:border-accent/50 hover:text-text"
                  : "btn-primary"
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
      </div>
    </div>
  );
}
