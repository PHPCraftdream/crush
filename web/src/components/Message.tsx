import { useState, useRef, useEffect, useMemo, useCallback, memo } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";
import type { Message as Msg, ContentPart } from "../types";
import { BrainCircuit, Check, Copy, GitFork, Pencil, RotateCcw, Star, Trash2, BookMarked } from "lucide-react";
import {
  $busySessions,
  toggleMessageSelection,
  updateMessageContent,
  updateMessageThinking,
  deleteMessagePart,
  rerunFromMessage,
  togglePinMessage,
} from "../store";
import { ConfirmDialog } from "./ConfirmDialog";
import { ForkSessionModal } from "./ForkSessionModal";

// Stable arrays — never recreated
const MD_REMARK = [remarkGfm, remarkBreaks];
const MD_REHYPE = [rehypeHighlight];

// ── Tiny utilities ────────────────────────────────────────────────────────────

function formatDuration(s: number) {
  if (s < 60) return `${s.toFixed(1)}s`;
  return `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`;
}

function extractText(parts: ContentPart[]) {
  return parts.filter(p => p.type === "text").map(p => (p as any).Text).join("\n");
}

function extractThinking(parts: ContentPart[]) {
  return parts.filter(p => p.type === "thinking").map(p => (p as any).Thinking).join("\n");
}

function formatJSON(s: string) {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

// ── Leaf components ───────────────────────────────────────────────────────────

const DurationBadge = memo(function DurationBadge({ message }: { message: Msg }) {
  const isFinished = useMemo(() => message.Parts.some(p => p.type === "finish"), [message.Parts]);
  const busy = useStore($busySessions);
  const isLive = !isFinished && busy.has(message.SessionID);
  const [elapsed, setElapsed] = useState(0);

  useEffect(() => {
    if (!isLive) return;
    const start = message.CreatedAt;
    const tick = () => setElapsed(Date.now() / 1000 - start);
    tick();
    const id = setInterval(tick, 100);
    return () => clearInterval(id);
  }, [isLive, message.CreatedAt]);

  const duration = isFinished || !isLive ? message.UpdatedAt - message.CreatedAt : elapsed;
  if (duration < 0.5) return null;
  return <span className="text-xs text-text-subtle font-mono tabular-nums">{formatDuration(duration)}</span>;
});

const CopyButton = memo(function CopyButton({ text, className = "", label = "Copy" }: { text: string; className?: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = useCallback(() => {
    navigator.clipboard.writeText(text).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); });
  }, [text]);
  return (
    <button onClick={copy} title={label} className={`btn-copy ${className}`}>
      {copied ? <><Check size={14} className="text-green" /><span className="text-green">Copied</span></> : <><Copy size={14} /><span>{label}</span></>}
    </button>
  );
});

// ── EditForm — owns all editing state and handlers ────────────────────────────

const EditForm = memo(function EditForm({
  initialValue,
  rows,
  className,
  onSave,
  onCancel,
}: {
  initialValue: string;
  rows: number;
  className: string;
  onSave: (text: string) => void;
  onCancel: () => void;
}) {
  const [value, setValue] = useState(initialValue);
  const taRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (taRef.current) {
      taRef.current.focus();
      taRef.current.selectionStart = taRef.current.value.length;
      taRef.current.style.height = "auto";
      taRef.current.style.height = taRef.current.scrollHeight + "px";
    }
  }, []);

  const onInput = useCallback((e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setValue(e.target.value);
    e.target.style.height = "auto";
    e.target.style.height = e.target.scrollHeight + "px";
  }, []);

  const commit = useCallback(() => onSave(value), [onSave, value]);

  const onKey = useCallback((e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Escape") onCancel();
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) onSave(value);
  }, [onSave, onCancel, value]);

  return (
    <div className="flex flex-col gap-2">
      <textarea ref={taRef} value={value} onChange={onInput} onKeyDown={onKey} rows={rows} className={className} style={{ overflow: "hidden" }} />
      <div className="flex gap-2">
        <button onClick={onCancel} className="btn-ghost-sm">Cancel</button>
        <button onClick={commit} className="px-3 py-1 text-xs btn-primary">Save</button>
      </div>
    </div>
  );
});

// ── Hover action strips — only mounted when hovered ───────────────────────────

const UserHoverActions = memo(function UserHoverActions({
  messageID, copyText, isLastUserMsg, isPinned, onEdit, onDelete, onFork,
}: {
  messageID: string; copyText: string; isLastUserMsg: boolean; isPinned: boolean;
  onEdit: () => void; onDelete: () => void; onFork: () => void;
}) {
  const handleRerun = useCallback(() => rerunFromMessage(messageID), [messageID]);
  const handlePin   = useCallback(() => togglePinMessage(messageID, !isPinned), [messageID, isPinned]);
  return (
    <div className="flex items-center gap-1.5">
      {copyText && <CopyButton text={copyText} />}
      {isLastUserMsg && <button onClick={handleRerun} title="Rerun" className="btn-icon"><RotateCcw size={13} /></button>}
      <button onClick={handlePin}   title={isPinned ? "Unpin" : "Pin message"} className={`p-1.5 transition-colors rounded ${isPinned ? "text-yellow" : "text-text-subtle hover:text-yellow"}`}><Star size={13} fill={isPinned ? "currentColor" : "none"} /></button>
      <button onClick={onFork}      title="Fork session"                       className="btn-icon"><GitFork size={13} /></button>
      <button onClick={onEdit}      title="Edit"                               className="btn-icon"><Pencil  size={13} /></button>
      <button onClick={onDelete}    title="Delete"                             className="btn-icon-danger"><Trash2 size={13} /></button>
    </div>
  );
});

const AssistantHoverActions = memo(function AssistantHoverActions({
  message, copyText, copyAll, onEdit, onDelete, onFork,
}: {
  message: Msg; copyText: string; copyAll: string;
  onEdit: () => void; onDelete: () => void; onFork: () => void;
}) {
  const handlePin = useCallback(() => togglePinMessage(message.ID, !message.Pinned), [message.ID, message.Pinned]);
  return (
    <div className="flex items-center gap-1 w-full">
      <div className="flex items-center gap-1.5">
        {copyText && <CopyButton text={copyText} />}
        {copyAll  && <CopyButton text={copyAll}  label="Copy all" />}
        <button onClick={handlePin} title={message.Pinned ? "Unpin" : "Pin message"} className={`p-1.5 transition-colors rounded ${message.Pinned ? "text-yellow" : "text-text-subtle hover:text-yellow"}`}><Star size={13} fill={message.Pinned ? "currentColor" : "none"} /></button>
        <button onClick={onFork}    title="Fork session" className="btn-icon"><GitFork size={13} /></button>
        <button onClick={onEdit}    title="Edit"         className="btn-icon"><Pencil  size={13} /></button>
        <button onClick={onDelete}  title="Delete"       className="btn-icon-danger"><Trash2 size={13} /></button>
      </div>
      <div className="flex items-center gap-2 ml-auto">
        <DurationBadge message={message} />
        {message.Model && <span className="text-xs text-text-subtle font-mono">{message.Model}</span>}
      </div>
    </div>
  );
});

// ── Tool blocks ───────────────────────────────────────────────────────────────

const ToolCallBlock = memo(function ToolCallBlock({ name, input, finished }: { name: string; input: string; finished: boolean }) {
  const formatted = useMemo(() => formatJSON(input), [input]);
  return (
    <div data-test-id="tool-call" className="tool-block my-2">
      <div className="flex items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2">
          <span className="text-xs text-text-subtle">⚡</span>
          <span className="text-mauve font-semibold text-sm">{name}</span>
          {!finished && <span data-test-id="tool-call-running" className="text-text-subtle text-xs animate-pulse">running…</span>}
        </div>
        <CopyButton text={input} />
      </div>
      <pre className="tool-output">{formatted}</pre>
    </div>
  );
});

const ToolResultBlock = memo(function ToolResultBlock({ name, content, isError }: { name: string; content: string; isError: boolean }) {
  return (
    <div data-test-id="tool-result" className="tool-block my-2 opacity-80">
      <div className="flex items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2">
          <span className="text-xs text-text-subtle">↩</span>
          <span className="text-text-muted font-semibold text-sm">{name}</span>
          {isError && <span data-test-id="tool-result-error" className="badge-error">error</span>}
        </div>
        <CopyButton text={content} />
      </div>
      <pre className="tool-output">{content}</pre>
    </div>
  );
});

const TextBlock = memo(function TextBlock({ text, isUser }: { text: string; isUser: boolean }) {
  if (isUser) return <span className="whitespace-pre-wrap">{text}</span>;
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
    </div>
  );
});

// ── ThinkingPart — owns its own edit/delete state ─────────────────────────────

const ThinkingPart = memo(function ThinkingPart({ thinking, messageID, partIndex, done }: { thinking: string; messageID: string; partIndex: number; done: boolean }) {
  const [editing, setEditing] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const closeEdit  = useCallback(() => setEditing(false), []);
  const openDel    = useCallback((e: React.MouseEvent) => { e.preventDefault(); e.stopPropagation(); setConfirmDelete(true); }, []);
  const openEditEv = useCallback((e: React.MouseEvent) => { e.preventDefault(); e.stopPropagation(); setEditing(true); }, []);

  const handleSave = useCallback((text: string) => {
    if (text && text !== thinking) updateMessageThinking(messageID, text);
    setEditing(false);
  }, [thinking, messageID]);

  const handleConfirmDelete = useCallback(() => {
    deleteMessagePart(messageID, partIndex);
    setConfirmDelete(false);
  }, [messageID, partIndex]);

  if (!done) {
    return (
      <div data-test-id="thinking-card" className="thinking-card">
        <div className="thinking-card-header">
          <BrainCircuit size={15} className="text-accent/70 shrink-0 animate-pulse" />
          <span data-test-id="thinking-label">Thinking…</span>
        </div>
        {thinking && (
          <pre data-test-id="thinking-content" className="px-4 pb-3 font-mono whitespace-pre-wrap text-text-subtle leading-relaxed max-h-40 overflow-y-auto border-t border-surface/50" style={{ fontSize: "var(--chat-font-size)" }}>
            {thinking}
          </pre>
        )}
      </div>
    );
  }

  return (
    <details data-test-id="thinking-card" className="thinking-card-done group">
      <summary data-test-id="thinking-toggle" className="thinking-toggle">
        <span className="text-accent/70"><BrainCircuit size={18} /></span>
        <span data-test-id="thinking-label">Thoughts</span>
        <div className="ml-auto flex items-center gap-0.5 hover-reveal">
          <CopyButton text={thinking} className="px-1.5 py-1 text-xs" />
          <button onClick={openEditEv} title="Edit thinking"   className="btn-icon-sm"><Pencil size={13} /></button>
          <button onClick={openDel}    title="Delete thinking" className="btn-icon-sm-danger"><Trash2 size={13} /></button>
        </div>
      </summary>
      {confirmDelete && (
        <ConfirmDialog
          title="Delete thinking"
          message="The model's reasoning will be removed from this message. This cannot be undone."
          confirmLabel="Delete"
          onConfirm={handleConfirmDelete}
          onCancel={() => setConfirmDelete(false)}
        />
      )}
      {editing ? (
        <div className="p-4 bg-base-overlay border-t border-surface">
          <EditForm
            initialValue={thinking}
            rows={6}
            className="w-full bg-base-subtle border border-accent/40 text-text-muted rounded-lg px-4 py-3 text-[14px] font-mono leading-relaxed resize-none outline-none focus:border-accent"
            onSave={handleSave}
            onCancel={closeEdit}
          />
        </div>
      ) : (
        <pre data-test-id="thinking-content" className="p-5 bg-base-overlay font-mono whitespace-pre-wrap overflow-x-auto text-text-muted border-t border-surface leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
          {thinking}
        </pre>
      )}
    </details>
  );
});

// ── Part router ───────────────────────────────────────────────────────────────

const Part = memo(function Part({ part, index, isUser, messageID, thinkingDone }: { part: ContentPart; index: number; isUser: boolean; messageID: string; thinkingDone: boolean }) {
  switch (part.type) {
    case "text":     return <TextBlock text={part.Text} isUser={isUser} />;
    case "thinking": return <ThinkingPart thinking={part.Thinking} messageID={messageID} partIndex={index} done={thinkingDone} />;
    case "tool_call":    return <ToolCallBlock   name={part.Name} input={part.Input}     finished={part.Finished} />;
    case "tool_result":  return <ToolResultBlock name={part.Name} content={part.Content} isError={part.IsError}  />;
    case "finish": return null;
    default:       return null;
  }
});

// ── Message content areas — own editing state ─────────────────────────────────

const UserContent = memo(function UserContent({
  message, editing, onSaveEdit, onCancelEdit,
}: {
  message: Msg;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}) {
  if (editing) {
    return (
      <EditForm
        initialValue={extractText(message.Parts)}
        rows={1}
        className="msg-bubble-user-edit text-[18px] min-w-[300px]"
        onSave={onSaveEdit}
        onCancel={onCancelEdit}
      />
    );
  }
  return (
    <div className="msg-bubble-user" style={{ fontSize: "var(--chat-font-size)" }}>
      {message.Parts.map((part, i) => <Part key={i} part={part} index={i} isUser messageID={message.ID} thinkingDone={false} />)}
    </div>
  );
});

const AssistantContent = memo(function AssistantContent({
  message, thinkingDone, editing, onSaveEdit, onCancelEdit,
}: {
  message: Msg;
  thinkingDone: boolean;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}) {
  if (editing) {
    return (
      <EditForm
        initialValue={extractText(message.Parts)}
        rows={4}
        className="field-textarea text-[16px]"
        onSave={onSaveEdit}
        onCancel={onCancelEdit}
      />
    );
  }
  return (
    <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
      {message.Parts.map((part, i) => <Part key={i} part={part} index={i} isUser={false} messageID={message.ID} thinkingDone={thinkingDone} />)}
    </div>
  );
});

// ── SummaryMessage ────────────────────────────────────────────────────────────

const SummaryMessage = memo(function SummaryMessage({ message }: { message: Msg }) {
  const text = useMemo(() => extractText(message.Parts), [message.Parts]);
  const isFinished = useMemo(() => message.Parts.some(p => p.type === "finish"), [message.Parts]);
  const [editing, setEditing] = useState(false);

  const handleSave = useCallback((newText: string) => {
    if (newText && newText !== text) updateMessageContent(message.ID, newText);
    setEditing(false);
  }, [message.ID, text]);

  return (
    <div className="px-8 py-3">
      <div className="summary-card">
        <div className="summary-header">
          <BookMarked size={15} className="text-yellow shrink-0" />
          <span className="text-sm font-semibold text-yellow">Context condensed</span>
          <span className="ml-auto text-xs text-text-muted font-mono">{message.Model}</span>
          {isFinished && <DurationBadge message={message} />}
          {isFinished && (
            <button onClick={() => setEditing(e => !e)} title="Edit summary" className="btn-icon-sm ml-1">
              <Pencil size={13} />
            </button>
          )}
        </div>
        <details className="group">
          <summary className="summary-toggle">
            <span className="group-open:hidden">Show summary ▸</span>
            <span className="hidden group-open:inline">Hide summary ▾</span>
          </summary>
          {editing ? (
            <div className="summary-body chat-font">
              <EditForm
                initialValue={text}
                rows={4}
                className="field-textarea"
                onSave={handleSave}
                onCancel={() => setEditing(false)}
              />
            </div>
          ) : text ? (
            <div className="summary-body md">
              <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
            </div>
          ) : null}
        </details>
      </div>
    </div>
  );
});

// ── Message ───────────────────────────────────────────────────────────────────

export interface MessageProps {
  message: Msg;
  onDeleteRequest: (id: string) => void;
  onRangeSelect: (index: number) => void;
  selectionActive: boolean;
  isLastUserMsg: boolean;
  isSelected: boolean;
  forkDefaultTitle: string;
  sessionID: string;
  index: number;
}

export const Message = memo(function Message({
  message, onDeleteRequest, onRangeSelect, selectionActive, isLastUserMsg, isSelected, forkDefaultTitle, sessionID, index,
}: MessageProps) {
  if (message.Hidden) return null;
  if (message.IsSummaryMessage) return <SummaryMessage message={message} />;

  const isUser = message.Role === "user";

  const copyText     = useMemo(() => extractText(message.Parts), [message.Parts]);
  const copyThinking = useMemo(() => !isUser ? extractThinking(message.Parts) : "", [isUser, message.Parts]);
  const copyAll      = useMemo(() => {
    if (!copyThinking) return "";
    return copyText ? `<thinking>\n${copyThinking}\n</thinking>\n\n${copyText}` : copyThinking;
  }, [copyText, copyThinking]);
  const thinkingDone = useMemo(() => !isUser && message.Parts.some(p => p.type === "text" || p.type === "finish"), [isUser, message.Parts]);
  const hasContent   = useMemo(() => !isUser && message.Parts.some(p => ["text","tool_call","tool_result","finish"].includes(p.type)), [isUser, message.Parts]);

  const [editing, setEditing] = useState(false);
  const [forking, setForking] = useState(false);
  const [hovered, setHovered] = useState(false);

  const handleMouseEnter = useCallback(() => setHovered(true),  []);
  const handleMouseLeave = useCallback(() => setHovered(false), []);
  const handleForkOpen   = useCallback(() => setForking(true),  []);
  const handleForkClose  = useCallback(() => setForking(false), []);
  const handleEditOpen   = useCallback(() => setEditing(true),  []);
  const handleEditClose  = useCallback(() => setEditing(false), []);
  const handleDelete     = useCallback(() => onDeleteRequest(message.ID), [onDeleteRequest, message.ID]);

  const handleSaveEdit = useCallback((text: string) => {
    if (text && text !== extractText(message.Parts)) updateMessageContent(message.ID, text);
    setEditing(false);
  }, [message.ID, message.Parts]);

  const handleCheckboxClick = useCallback((e: React.MouseEvent) => {
    if (e.shiftKey) {
      e.preventDefault(); // Prevent text selection between clicks
      onRangeSelect(index);
    }
    else { toggleMessageSelection(message.ID); }
  }, [message.ID, index, onRangeSelect]);

  // Checkbox is always in DOM (reserves layout space), opacity-0 when not relevant
  const checkboxVisible = selectionActive || isSelected || hovered;

  return (
    <div
      className={`flex flex-col px-8 py-3 transition-colors ${isSelected ? "bg-accent/5" : ""} ${message.Pinned ? "border-l-4 border-yellow/60 bg-yellow/[5%]" : ""}`}
      onMouseEnter={handleMouseEnter}
      onMouseLeave={handleMouseLeave}
    >
      <div className={`flex gap-3 ${isUser ? "justify-end" : "justify-start"}`}>
        <div
          className={`msg-checkbox-wrap ${checkboxVisible ? "opacity-100" : "opacity-0 pointer-events-none"}`}
          style={{ order: isUser ? 1 : -1 }}
          onClick={handleCheckboxClick}
        >
          <div className={`msg-checkbox ${isSelected ? "bg-accent border-accent" : "border-text-subtle/50 hover:border-accent"}`}>
            {isSelected && <Check size={10} className="text-white shrink-0" />}
          </div>
        </div>
        {isUser ? (
          <div className="max-w-[80%]">
            <UserContent message={message} editing={editing} onSaveEdit={handleSaveEdit} onCancelEdit={handleEditClose} />
          </div>
        ) : (
          <div className="w-full max-w-[92%]">
            <AssistantContent message={message} thinkingDone={thinkingDone} editing={editing} onSaveEdit={handleSaveEdit} onCancelEdit={handleEditClose} />
          </div>
        )}
      </div>

      {/* Action strip — fixed-height row; interactive controls only mounted on hover */}
      {!editing && (
        <div className={`msg-actions ${isUser ? "justify-end" : "justify-start"}`}>
          {/* Star is always visible for pinned messages, regardless of hover state */}
          {message.Pinned && (
            <Star
              size={13}
              className={`text-yellow shrink-0 ${isUser ? "order-last ml-2" : "order-first mr-2"}`}
              fill="currentColor"
            />
          )}
          {hovered && (
            isUser ? (
              <UserHoverActions
                messageID={message.ID}
                copyText={copyText}
                isLastUserMsg={isLastUserMsg}
                isPinned={message.Pinned}
                onEdit={handleEditOpen}
                onDelete={handleDelete}
                onFork={handleForkOpen}
              />
            ) : hasContent ? (
              <AssistantHoverActions
                message={message}
                copyText={copyText}
                copyAll={copyAll}
                onEdit={handleEditOpen}
                onDelete={handleDelete}
                onFork={handleForkOpen}
              />
            ) : null
          )}
        </div>
      )}

      {forking && (
        <ForkSessionModal sessionID={sessionID} defaultTitle={forkDefaultTitle} onClose={handleForkClose} />
      )}
    </div>
  );
});
