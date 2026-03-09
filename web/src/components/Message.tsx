import { useState, useRef, useEffect } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";
import type { Message as Msg, ContentPart } from "../types";
import { BrainCircuit, Check, Copy, Pencil, Trash2 } from "lucide-react";
import {
  $selectedMessageIDs,
  toggleMessageSelection,
  updateMessageContent,
} from "../store";

function CopyButton({ text, className = "" }: { text: string; className?: string }) {
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
      title="Copy"
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
          <span>Copy</span>
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

interface MessageProps {
  message: Msg;
  onDeleteRequest: (id: string) => void;
  selectionActive: boolean;
}

export function Message({ message, onDeleteRequest, selectionActive }: MessageProps) {
  const isUser = message.Role === "user";
  const copyText = extractText(message.Parts);
  const selectedIDs = useStore($selectedMessageIDs);
  const isSelected = selectedIDs.has(message.ID);

  const [editing, setEditing] = useState(false);
  const [editValue, setEditValue] = useState("");
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
                className="bg-accent/10 border border-accent/40 text-text rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] leading-relaxed resize-none outline-none focus:border-accent w-full min-w-[300px]"
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
                  className="px-3 py-1 text-xs bg-accent text-white rounded-lg hover:opacity-90 transition-opacity"
                >
                  Save
                </button>
              </div>
            </div>
          ) : (
            <>
              <div className="bg-accent text-white rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] leading-relaxed shadow-md">
                {message.Parts.map((part, i) => (
                  <Part key={i} part={part} isUser />
                ))}
              </div>
              <div className="flex items-center justify-between mt-1.5 gap-2">
                {copyText && <CopyButton text={copyText} className="text-text-subtle" />}
                <div className="flex items-center gap-1 ml-auto opacity-0 group-hover/msg:opacity-100 transition-opacity">
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
                  className="px-3 py-1 text-xs bg-accent text-white rounded-lg hover:opacity-90 transition-opacity"
                >
                  Save <span className="opacity-70">(Ctrl+Enter)</span>
                </button>
              </div>
            </div>
          ) : (
            <>
              <div className="text-text text-[17px] leading-relaxed">
                {message.Parts.map((part, i) => (
                  <Part key={i} part={part} isUser={false} />
                ))}
              </div>
              <div className="flex items-center justify-between mt-3">
                {copyText && <CopyButton text={copyText} />}
                <div className="flex items-center gap-1 ml-auto">
                  <div className="flex items-center gap-1 opacity-0 group-hover/msg:opacity-100 transition-opacity">
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
                  {message.Model && (
                    <span className="text-xs text-text-subtle font-mono ml-2">
                      {message.Model}
                    </span>
                  )}
                </div>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}

function Part({ part, isUser }: { part: ContentPart; isUser: boolean }) {
  switch (part.type) {
    case "text":
      return isUser ? (
        <span className="whitespace-pre-wrap">{part.Text}</span>
      ) : (
        <div className="md">
          <ReactMarkdown
            remarkPlugins={[remarkGfm]}
            rehypePlugins={[rehypeHighlight]}
          >
            {part.Text}
          </ReactMarkdown>
        </div>
      );

    case "thinking":
      return (
        <details className="my-3 border border-surface rounded-xl overflow-hidden shadow-sm">
          <summary className="px-5 py-3 cursor-pointer select-none text-base text-text-muted bg-base-subtle hover:bg-base-overlay transition-colors flex items-center gap-2.5 font-medium">
            <span className="text-accent/70"><BrainCircuit size={18} /></span>
            <span>Thinking…</span>
          </summary>
          <pre className="p-5 bg-base-overlay text-[14px] font-mono whitespace-pre-wrap overflow-x-auto text-text-muted border-t border-surface leading-relaxed">
            {part.Thinking}
          </pre>
        </details>
      );

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
