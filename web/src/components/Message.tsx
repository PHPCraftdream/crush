import { useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";
import type { Message as Msg, ContentPart } from "../types";

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
      className={`inline-flex items-center gap-1 text-xs text-text-subtle hover:text-text transition-colors ${className}`}
    >
      {copied ? (
        <span className="text-green">✓ Copied</span>
      ) : (
        <span>⎘ Copy</span>
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
    <div className={`group flex px-6 py-3 ${isUser ? "justify-end" : "justify-start"}`}>
      {isUser ? (
        /* User message — bubble on the right */
        <div className="relative max-w-[75%]">
          <div className="bg-accent text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm leading-relaxed shadow-sm">
            {message.Parts.map((part, i) => (
              <Part key={i} part={part} isUser />
            ))}
          </div>
          {copyText && (
            <div className="flex justify-end mt-1 opacity-0 group-hover:opacity-100 transition-opacity">
              <CopyButton
                text={copyText}
                className="text-text-subtle"
              />
            </div>
          )}
        </div>
      ) : (
        /* Assistant message — full width on the left */
        <div className="relative w-full max-w-[90%]">
          <div className="text-text text-sm leading-relaxed">
            {message.Parts.map((part, i) => (
              <Part key={i} part={part} isUser={false} />
            ))}
          </div>
          {copyText && (
            <div className="flex mt-2 opacity-0 group-hover:opacity-100 transition-opacity">
              <CopyButton text={copyText} />
            </div>
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
        <details className="my-2 border border-surface rounded-lg overflow-hidden">
          <summary className="px-4 py-2.5 cursor-pointer select-none text-sm text-text-muted bg-base-subtle hover:bg-base-overlay transition-colors flex items-center gap-2">
            <span className="text-text-subtle">🤔</span>
            <span>Thinking…</span>
          </summary>
          <pre className="p-4 bg-base-overlay text-xs font-mono whitespace-pre-wrap overflow-x-auto text-text-muted border-t border-surface">
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
