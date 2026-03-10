import { useState, useRef, useEffect } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";
import type { Message as Msg, ContentPart } from "../types";
import { BrainCircuit, Check, Copy, GitFork, Pencil, RotateCcw, Trash2, BookMarked } from "lucide-react";
import {
  $busySessions,
  $selectedMessageIDs,
  $sessions,
  $activeSessionID,
  toggleMessageSelection,
  updateMessageContent,
  updateMessageThinking,
  deleteMessagePart,
  rerunFromMessage,
} from "../store";
import { ConfirmDialog } from "./ConfirmDialog";
import { ForkSessionModal } from "./ForkSessionModal";

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds.toFixed(1)}s`;
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return `${m}m ${s}s`;
}

function DurationBadge({ message }: { message: Msg }) {
  const isFinished = message.Parts.some((p) => p.type === "finish");
  const busySessions = useStore($busySessions);
  // Only tick when the session is actively processing — old interrupted messages
  // have no finish part but are also not running anymore.
  const isLive = !isFinished && busySessions.has(message.SessionID);
  const [elapsed, setElapsed] = useState(0);

  useEffect(() => {
    if (!isLive) return;
    const start = message.CreatedAt; // Unix seconds
    const tick = () => setElapsed(Date.now() / 1000 - start);
    tick();
    const id = setInterval(tick, 100);
    return () => clearInterval(id);
  }, [isLive, message.CreatedAt]);

  const duration = isFinished || !isLive
    ? message.UpdatedAt - message.CreatedAt
    : elapsed;
  if (duration < 0.5) return null;

  return (
    <span className="text-xs text-text-subtle font-mono tabular-nums" title="Generation time">
      {formatDuration(duration)}
    </span>
  );
}

function CopyButton({ text, className = "", label = "Copy" }: { text: string; className?: string; label?: string }) {
  const [copied, setCopied] = useState(false);

  function copy() {
    navigator.clipboard.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }

  return (
    <button
      onClick={copy}
      title={label}
      className={`inline-flex items-center gap-1.5 text-sm text-text-subtle hover:text-text transition-colors font-medium ${className}`}
    >
      {copied ? (
        <>
          <Check size={14} className="text-green" />
          <span className="text-green">Copied</span>
        </>
      ) : (
        <>
          <Copy size={14} />
          <span>{label}</span>
        </>
      )}
    </button>
  );
}

function extractText(parts: ContentPart[]): string {
  return parts
    .filter((p) => p.type === "text")
    .map((p) => (p as { type: "text"; Text: string }).Text)
    .join("\n");
}

function extractThinking(parts: ContentPart[]): string {
  return parts
    .filter((p) => p.type === "thinking")
    .map((p) => (p as { type: "thinking"; Thinking: string }).Thinking)
    .join("\n");
}

function extractAll(parts: ContentPart[]): string {
  const thinking = extractThinking(parts);
  const text = extractText(parts);
  if (thinking && text) return `<thinking>\n${thinking}\n</thinking>\n\n${text}`;
  return thinking || text;
}

interface MessageProps {
  message: Msg;
  onDeleteRequest: (id: string) => void;
  selectionActive: boolean;
  isLastUserMsg?: boolean;
}

function SummaryMessage({ message }: { message: Msg }) {
  const text = extractText(message.Parts);
  const isFinished = message.Parts.some((p) => p.type === "finish");

  return (
    <div className="px-8 py-3">
      <div className="rounded-2xl border border-yellow/30 bg-yellow/5 overflow-hidden">
        {/* Header */}
        <div className="flex items-center gap-2.5 px-5 py-3 border-b border-yellow/20 bg-yellow/8">
          <BookMarked size={15} className="text-yellow shrink-0" />
          <span className="text-sm font-semibold text-yellow">Context condensed</span>
          <span className="ml-auto text-xs text-text-muted font-mono">
            {message.Model}
          </span>
          {isFinished && <DurationBadge message={message} />}
        </div>
        {/* Summary content — collapsed by default */}
        <details className="group">
          <summary className="flex items-center gap-2 px-5 py-2.5 cursor-pointer text-xs text-text-muted hover:text-text transition-colors select-none list-none">
            <span className="group-open:hidden">Show summary ▸</span>
            <span className="hidden group-open:inline">Hide summary ▾</span>
          </summary>
          {text && (
            <div className="px-5 pb-4 text-[15px] leading-relaxed text-text-subtle border-t border-yellow/10 pt-3">
              <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} rehypePlugins={[rehypeHighlight]}>
                {text}
              </ReactMarkdown>
            </div>
          )}
        </details>
      </div>
    </div>
  );
}

export function Message({ message, onDeleteRequest, selectionActive, isLastUserMsg }: MessageProps) {
  if (message.IsSummaryMessage) {
    return <SummaryMessage message={message} />;
  }

  const isUser = message.Role === "user";
  const copyText = extractText(message.Parts);
  const copyThinking = !isUser ? extractThinking(message.Parts) : "";
  const copyAll = !isUser && copyThinking ? extractAll(message.Parts) : "";
  const selectedIDs = useStore($selectedMessageIDs);
  const isSelected = selectedIDs.has(message.ID);
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;

  const [editing, setEditing] = useState(false);
  const [editValue, setEditValue] = useState("");
  const [forking, setForking] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  function startEdit() {
    setEditValue(copyText);
    setEditing(true);
  }

  useEffect(() => {
    if (editing && textareaRef.current) {
      textareaRef.current.focus();
      textareaRef.current.selectionStart = textareaRef.current.value.length;
      // Auto-resize
      textareaRef.current.style.height = "auto";
      textareaRef.current.style.height = textareaRef.current.scrollHeight + "px";
    }
  }, [editing]);

  function commitEdit() {
    const trimmed = editValue.trim();
    if (trimmed && trimmed !== copyText) {
      updateMessageContent(message.ID, trimmed);
    }
    setEditing(false);
  }

  function cancelEdit() {
    setEditing(false);
  }

  function onEditKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Escape") cancelEdit();
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) commitEdit();
  }

  function onTextareaInput(e: React.ChangeEvent<HTMLTextAreaElement>) {
    setEditValue(e.target.value);
    e.target.style.height = "auto";
    e.target.style.height = e.target.scrollHeight + "px";
  }

  function handleCheckboxClick(e: React.MouseEvent) {
    e.stopPropagation();
  }

  return (
    <div
      className={`group/msg flex px-8 py-4 gap-3 transition-colors ${
        isSelected ? "bg-accent/5" : ""
      } ${isUser ? "justify-end" : "justify-start"}`}
    >
      {/* Checkbox — visible when any selection is active or on hover */}
      <div
        className={`flex items-start pt-1 shrink-0 transition-opacity ${
          selectionActive || isSelected ? "opacity-100" : "opacity-0 group-hover/msg:opacity-100"
        }`}
        style={{ order: isUser ? 1 : -1 }}
      >
        <input
          type="checkbox"
          checked={isSelected}
          onChange={() => toggleMessageSelection(message.ID)}
          onClick={handleCheckboxClick}
          className="w-4 h-4 accent-accent cursor-pointer mt-1"
        />
      </div>

      {isUser ? (
        /* User message — bubble on the right */
        <div className="relative max-w-[80%]">
          {editing ? (
            <div className="flex flex-col gap-2">
              <textarea
                ref={textareaRef}
                value={editValue}
                onChange={onTextareaInput}
                onKeyDown={onEditKey}
                rows={1}
                className="bg-accent/10 border border-accent/40 text-text rounded-2xl rounded-tr-sm px-5 py-3.5 text-[18px] leading-relaxed resize-none outline-none focus:border-accent w-full min-w-[300px]"
                style={{ overflow: "hidden" }}
              />
              <div className="flex gap-2 justify-end">
                <button
                  onClick={cancelEdit}
                  className="px-3 py-1 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
                >
                  Cancel
                </button>
                <button
                  onClick={commitEdit}
                  className="px-3 py-1 text-xs btn-primary"
                >
                  Save
                </button>
              </div>
            </div>
          ) : (
            <>
              <div className="bg-accent/25 dark:bg-base-overlay text-text rounded-2xl rounded-tr-sm px-5 py-3.5 leading-relaxed shadow-md border border-accent/20 dark:border dark:border-surface" style={{ fontSize: "var(--chat-font-size)" }}>
                {message.Parts.map((part, i) => (
                  <Part key={i} part={part} index={i} isUser messageID={message.ID} thinkingDone={false} />
                ))}
              </div>
              <div className="flex items-center justify-between mt-1.5 gap-2">
                {copyText && <CopyButton text={copyText} className="text-text-subtle" />}
                <div className="flex items-center gap-1 ml-auto">
                  {isLastUserMsg && (
                    <button
                      onClick={() => rerunFromMessage(message.ID)}
                      title="Rerun — delete agent response and resend"
                      className="p-1.5 text-text-subtle hover:text-accent transition-colors rounded"
                    >
                      <RotateCcw size={13} />
                    </button>
                  )}
                  <button
                    onClick={() => setForking(true)}
                    title="Fork session at this message"
                    className="p-1.5 text-text-subtle hover:text-accent transition-colors rounded"
                  >
                    <GitFork size={13} />
                  </button>
                  <button
                    onClick={startEdit}
                    title="Edit message"
                    className="p-1.5 text-text-subtle hover:text-accent transition-colors rounded"
                  >
                    <Pencil size={13} />
                  </button>
                  <button
                    onClick={() => onDeleteRequest(message.ID)}
                    title="Delete message"
                    className="p-1.5 text-text-subtle hover:text-red transition-colors rounded"
                  >
                    <Trash2 size={13} />
                  </button>
                </div>
              </div>
            </>
          )}
        </div>
      ) : (
        /* Assistant message — full width on the left */
        <div className="relative w-full max-w-[92%]">
          {editing ? (
            <div className="flex flex-col gap-2">
              <textarea
                ref={textareaRef}
                value={editValue}
                onChange={onTextareaInput}
                onKeyDown={onEditKey}
                rows={4}
                className="bg-base-overlay border border-accent/40 text-text rounded-xl px-4 py-3 text-[16px] leading-relaxed resize-none outline-none focus:border-accent w-full"
                style={{ overflow: "hidden" }}
              />
              <div className="flex gap-2">
                <button
                  onClick={cancelEdit}
                  className="px-3 py-1 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
                >
                  Cancel <span className="opacity-50">(Esc)</span>
                </button>
                <button
                  onClick={commitEdit}
                  className="px-3 py-1 text-xs btn-primary"
                >
                  Save <span className="opacity-70">(Ctrl+Enter)</span>
                </button>
              </div>
            </div>
          ) : (
            <>
              {(() => {
                const thinkingDone = message.Parts.some(p => p.type === "text" || p.type === "finish");
                const hasContent = message.Parts.some(p => p.type === "text" || p.type === "tool_call" || p.type === "tool_result" || p.type === "finish");
                return (
                  <>
                    <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
                      {message.Parts.map((part, i) => (
                        <Part key={i} part={part} index={i} isUser={false} messageID={message.ID} thinkingDone={thinkingDone} />
                      ))}
                    </div>
                    {/* Only show the action footer once there is real content */}
                    {hasContent && (
                      <div className="flex items-center justify-between mt-3">
                        <div className="flex items-center gap-3">
                          {copyText && <CopyButton text={copyText} />}
                          {copyAll && <CopyButton text={copyAll} label="Copy all" />}
                        </div>
                        <div className="flex items-center gap-1 ml-auto">
                          <div className="flex items-center gap-1">
                            <button
                              onClick={() => setForking(true)}
                              title="Fork session at this message"
                              className="p-1.5 text-text-subtle hover:text-accent transition-colors rounded"
                            >
                              <GitFork size={13} />
                            </button>
                            <button
                              onClick={startEdit}
                              title="Edit message"
                              className="p-1.5 text-text-subtle hover:text-accent transition-colors rounded"
                            >
                              <Pencil size={13} />
                            </button>
                            <button
                              onClick={() => onDeleteRequest(message.ID)}
                              title="Delete message"
                              className="p-1.5 text-text-subtle hover:text-red transition-colors rounded"
                            >
                              <Trash2 size={13} />
                            </button>
                          </div>
                          <DurationBadge message={message} />
                          {message.Model && (
                            <span className="text-xs text-text-subtle font-mono ml-2">
                              {message.Model}
                            </span>
                          )}
                        </div>
                      </div>
                    )}
                  </>
                );
              })()}
            </>
          )}
        </div>
      )}
      {forking && activeSessionID && (
        <ForkSessionModal
          sessionID={activeSessionID}
          defaultTitle={(activeSession?.Title || "Session") + " fork"}
          onClose={() => setForking(false)}
        />
      )}
    </div>
  );
}

function ThinkingPart({ thinking, messageID, partIndex, done }: { thinking: string; messageID: string; partIndex: number; done: boolean }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const taRef = useRef<HTMLTextAreaElement>(null);

  function startEdit() {
    setValue(thinking);
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
    const trimmed = value.trim();
    if (trimmed && trimmed !== thinking) {
      updateMessageThinking(messageID, trimmed);
    }
    setEditing(false);
  }

  function onKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Escape") setEditing(false);
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) save();
  }

  function onInput(e: React.ChangeEvent<HTMLTextAreaElement>) {
    setValue(e.target.value);
    e.target.style.height = "auto";
    e.target.style.height = e.target.scrollHeight + "px";
  }

  // While still thinking: show a compact live indicator with streaming content
  if (!done) {
    return (
      <div className="my-2 rounded-xl border border-surface bg-base-subtle overflow-hidden">
        <div className="flex items-center gap-2 px-4 py-2.5 text-sm text-text-muted font-medium">
          <BrainCircuit size={15} className="text-accent/70 shrink-0 animate-pulse" />
          <span>Thinking…</span>
        </div>
        {thinking && (
          <pre className="px-4 pb-3 font-mono whitespace-pre-wrap text-text-subtle leading-relaxed max-h-40 overflow-y-auto border-t border-surface/50" style={{ fontSize: "var(--chat-font-size)" }}>
            {thinking}
          </pre>
        )}
      </div>
    );
  }

  return (
    <details className="my-3 border border-surface rounded-xl overflow-hidden shadow-sm">
      <summary className="px-5 py-3 cursor-pointer select-none text-base text-text-muted bg-base-subtle hover:bg-base-overlay transition-colors flex items-center gap-2.5 font-medium">
        <span className="text-accent/70"><BrainCircuit size={18} /></span>
        <span>Thoughts</span>
        <div className="ml-auto flex items-center gap-0.5 opacity-0 group-hover/msg:opacity-100">
          <CopyButton text={thinking} className="px-1.5 py-1 text-xs" />
          <button
            onClick={(e) => { e.preventDefault(); e.stopPropagation(); startEdit(); }}
            title="Edit thinking"
            className="p-1 text-text-subtle hover:text-accent transition-colors rounded"
          >
            <Pencil size={13} />
          </button>
          <button
            onClick={(e) => { e.preventDefault(); e.stopPropagation(); setConfirmDelete(true); }}
            title="Delete thinking"
            className="p-1 text-text-subtle hover:text-red transition-colors rounded"
          >
            <Trash2 size={13} />
          </button>
        </div>
      </summary>
      {confirmDelete && (
        <ConfirmDialog
          title="Delete thinking"
          message="The model's reasoning will be removed from this message. This cannot be undone."
          confirmLabel="Delete"
          onConfirm={() => { deleteMessagePart(messageID, partIndex); setConfirmDelete(false); }}
          onCancel={() => setConfirmDelete(false)}
        />
      )}
      {editing ? (
        <div className="p-4 bg-base-overlay border-t border-surface">
          <textarea
            ref={taRef}
            value={value}
            onChange={onInput}
            onKeyDown={onKey}
            className="w-full bg-base-subtle border border-accent/40 text-text-muted rounded-lg px-4 py-3 text-[14px] font-mono leading-relaxed resize-none outline-none focus:border-accent"
            style={{ overflow: "hidden" }}
          />
          <div className="flex gap-2 mt-2">
            <button
              onClick={() => setEditing(false)}
              className="px-3 py-1 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
            >
              Cancel <span className="opacity-50">(Esc)</span>
            </button>
            <button
              onClick={save}
              className="px-3 py-1 text-xs btn-primary"
            >
              Save <span className="opacity-70">(Ctrl+Enter)</span>
            </button>
          </div>
        </div>
      ) : (
        <pre className="p-5 bg-base-overlay font-mono whitespace-pre-wrap overflow-x-auto text-text-muted border-t border-surface leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
          {thinking}
        </pre>
      )}
    </details>
  );
}

function Part({ part, index, isUser, messageID, thinkingDone }: { part: ContentPart; index: number; isUser: boolean; messageID: string; thinkingDone: boolean }) {
  switch (part.type) {
    case "text":
      return isUser ? (
        <span className="whitespace-pre-wrap">{part.Text}</span>
      ) : (
        <div className="md">
          <ReactMarkdown
            remarkPlugins={[remarkGfm, remarkBreaks]}
            rehypePlugins={[rehypeHighlight]}
          >
            {part.Text}
          </ReactMarkdown>
        </div>
      );

    case "thinking":
      return <ThinkingPart thinking={part.Thinking} messageID={messageID} partIndex={index} done={thinkingDone} />;

    case "tool_call":
      return (
        <div className="tool-block my-2">
          <div className="flex items-center justify-between gap-2 mb-2">
            <div className="flex items-center gap-2">
              <span className="text-xs text-text-subtle">⚡</span>
              <span className="text-mauve font-semibold text-sm">{part.Name}</span>
              {!part.Finished && (
                <span className="text-text-subtle text-xs animate-pulse">running…</span>
              )}
            </div>
            <CopyButton text={part.Input} />
          </div>
          <pre className="text-text-muted text-xs whitespace-pre-wrap overflow-x-auto max-h-48 leading-relaxed">
            {formatJSON(part.Input)}
          </pre>
        </div>
      );

    case "tool_result":
      return (
        <div className="tool-block my-2 opacity-80">
          <div className="flex items-center justify-between gap-2 mb-2">
            <div className="flex items-center gap-2">
              <span className="text-xs text-text-subtle">↩</span>
              <span className="text-text-muted font-semibold text-sm">{part.Name}</span>
              {part.IsError && (
                <span className="bg-red/10 text-red border border-red/20 text-xs font-medium rounded-full px-2 py-0.5">
                  error
                </span>
              )}
            </div>
            <CopyButton text={part.Content} />
          </div>
          <pre className="text-text-muted text-xs whitespace-pre-wrap overflow-x-auto max-h-48 leading-relaxed">
            {part.Content}
          </pre>
        </div>
      );

    case "finish":
      return null;
    default:
      return null;
  }
}

function formatJSON(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
