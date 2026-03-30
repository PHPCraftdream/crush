import { memo, useMemo } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";
import { Bot } from "lucide-react";
import { $subAgentMessages, $busySessions } from "../store";
import type { Message, ContentPart } from "../types";

const MD_REMARK = [remarkGfm, remarkBreaks];
const MD_REHYPE = [rehypeHighlight];

function extractTextFromParts(parts: ContentPart[]): string {
  return parts
    .filter((p) => p.type === "text")
    .map((p) => (p as { type: "text"; Text: string }).Text)
    .join("\n");
}

function isFinished(msg: Message): boolean {
  return msg.Parts.some((p) => p.type === "finish");
}

const SubAgentMessage = memo(function SubAgentMessage({ message }: { message: Message }) {
  const text = useMemo(() => extractTextFromParts(message.Parts), [message.Parts]);
  const toolCalls = useMemo(
    () => message.Parts.filter((p) => p.type === "tool_call") as Array<{ type: "tool_call"; Name: string; Input: string; Finished: boolean }>,
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
  const subSessionID = `${messageID}$$${toolCallID}`;
  const allSubMessages = useStore($subAgentMessages);
  const busySessions = useStore($busySessions);
  const messages = allSubMessages.get(subSessionID) ?? [];
  const isRunning = busySessions.has(subSessionID);

  const done = useMemo(
    () => messages.some((m) => m.Role === "assistant" && isFinished(m)),
    [messages],
  );

  const label = useMemo(() => {
    const maxLen = 80;
    const text = prompt.length > maxLen ? prompt.slice(0, maxLen) + "..." : prompt;
    return text;
  }, [prompt]);

  return (
    <details open={!done} className="sub-agent-block my-2">
      <summary className="sub-agent-toggle">
        <Bot size={15} className={`shrink-0 ${isRunning ? "text-accent animate-pulse" : "text-text-subtle"}`} />
        <span className="font-semibold text-sm">{done ? "Agent" : "Agent"}</span>
        {isRunning && <span className="text-xs text-text-subtle animate-pulse">running...</span>}
        {done && <span className="text-xs text-green font-medium">done</span>}
        <span className="text-xs text-text-muted truncate ml-1 max-w-[400px]">{label}</span>
      </summary>
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
    </details>
  );
});
