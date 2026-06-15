import { memo, useEffect, useMemo, useRef, useState } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";
import { Bot } from "lucide-react";
import { $subAgentMessages, $busySessions } from "../store";
import { ws } from "../ws";
import type { Message, ContentPart } from "../types";

const MD_REMARK = [remarkGfm, remarkBreaks];
const MD_REHYPE = [rehypeHighlight];

function extractTextFromParts(parts?: ContentPart[]): string {
  if (!parts) return "";
  return parts
    .filter((p) => p.type === "text")
    .map((p) => (p as { type: "text"; Text: string }).Text)
    .join("\n");
}

function isFinished(msg: Message): boolean {
  return msg.Parts?.some((p) => p.type === "finish") ?? false;
}

const SubAgentMessage = memo(function SubAgentMessage({ message }: { message: Message }) {
  const text = useMemo(() => extractTextFromParts(message.Parts), [message.Parts]);
  const toolCalls = useMemo(
    () => (message.Parts ?? []).filter((p) => p.type === "tool_call") as Array<{ type: "tool_call"; Name: string; Input: string; Finished: boolean }>,
    [message.Parts],
  );

  if (!text && toolCalls.length === 0) return null;

  return (
    <div className="py-1">
      {toolCalls.map((tc, i) => (
        <div key={i} className="flex items-center gap-1.5 text-xs text-text-subtle py-0.5">
          <span className="text-mauve font-semibold">{tc.Name}</span>
          {!tc.Finished && <span className="animate-pulse">running...</span>}
        </div>
      ))}
      {text && (
        <div className="md text-sm text-text-muted">
          <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
        </div>
      )}
    </div>
  );
});

export const SubAgentBlock = memo(function SubAgentBlock({
  messageID,
  toolCallID,
  prompt,
}: {
  messageID: string;
  toolCallID: string;
  prompt: string;
}) {
  // Backend creates the sub-agent session with `ID = toolCallID`
  // (see internal/session/session.go CreateTaskSession), so the
  // sub-session is keyed by the spawning tool_call ID directly.
  // The `messageID` prop is unused but kept for future linking.
  void messageID;
  const subSessionID = toolCallID;
  const allSubMessages = useStore($subAgentMessages);
  const busySessions = useStore($busySessions);
  const messages = allSubMessages.get(subSessionID) ?? [];
  const isRunning = busySessions.has(subSessionID);

  const done = useMemo(
    () => messages.some((m) => m.Role === "assistant" && isFinished(m)),
    [messages],
  );

  const label = useMemo(() => {
    if (!prompt) return "";
    const maxLen = 80;
    const text = prompt.length > maxLen ? prompt.slice(0, maxLen) + "..." : prompt;
    return text;
  }, [prompt]);

  // Lazy-load sub-agent messages on first mount when nothing is in the
  // store yet (the WS handler only auto-loads sub-sessions created during
  // the live session — past runs surfaced after a reload start empty).
  const requested = useRef(false);
  useEffect(() => {
    if (requested.current) return;
    if (messages.length > 0) return;
    requested.current = true;
    ws.send("load_messages", { sessionID: subSessionID });
  }, [subSessionID, messages.length]);

  // Open while the sub-agent is still working (mirrors prior `open={!done}`
  // behaviour); the user's manual toggle wins once they touch the chevron.
  const [override, setOverride] = useState<boolean | undefined>(undefined);
  const open = override ?? !done;
  const prevDone = useRef(done);
  useEffect(() => {
    if (done && !prevDone.current) setOverride(undefined);
    prevDone.current = done;
  }, [done]);
  const toggle = () => setOverride(!open);

  return (
    <div className="sub-agent-block my-2">
      <button
        type="button"
        onClick={toggle}
        aria-expanded={open}
        className="sub-agent-toggle w-full text-left bg-transparent border-0"
      >
        <Bot size={15} className={`shrink-0 ${isRunning ? "text-accent animate-pulse" : "text-text-subtle"}`} />
        <span className="font-semibold text-sm">{done ? "Agent" : "Agent"}</span>
        {isRunning && <span className="text-xs text-text-subtle animate-pulse">running...</span>}
        {done && <span className="text-xs text-green font-medium">done</span>}
        <span className="text-xs text-text-muted truncate ml-1 max-w-[400px]">{label}</span>
      </button>
      {open && (
        <div className="sub-agent-body">
          {messages.length === 0 && isRunning && (
            <div className="text-xs text-text-subtle animate-pulse py-2">Starting agent...</div>
          )}
          {messages
            .filter((m) => m.Role === "assistant")
            .map((m) => (
              <SubAgentMessage key={m.ID} message={m} />
            ))}
        </div>
      )}
    </div>
  );
});
