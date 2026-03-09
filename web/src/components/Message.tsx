import { useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";
import type { Message as Msg, ContentPart } from "../types";
import { BrainCircuit, Check, Copy } from "lucide-react";

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

export function Message({ message }: { message: Msg }) {
  const isUser = message.Role === "user";
  const copyText = extractText(message.Parts);

  return (
    <div className={`group flex px-8 py-4 ${isUser ? "justify-end" : "justify-start"}`}>
      {isUser ? (
        /* User message — bubble on the right */
        <div className="relative max-w-[80%]">
          <div className="bg-accent text-white rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] leading-relaxed shadow-md">
            {message.Parts.map((part, i) => (
              <Part key={i} part={part} isUser />
            ))}
          </div>
          {copyText && (
            <div className="flex justify-end mt-1.5">
              <CopyButton
                text={copyText}
                className="text-text-subtle"
              />
            </div>
          )}
        </div>
      ) : (
        /* Assistant message — full width on the left */
        <div className="relative w-full max-w-[92%]">
          <div className="text-text text-[17px] leading-relaxed">
            {message.Parts.map((part, i) => (
              <Part key={i} part={part} isUser={false} />
            ))}
          </div>
          <div className="flex items-center justify-between mt-3">
            {copyText && <CopyButton text={copyText} />}
            {message.Model && (
              <span className="text-xs text-text-subtle font-mono ml-auto">
                {message.Model}
              </span>
            )}
          </div>
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
