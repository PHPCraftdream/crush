import { useEffect, useRef } from "react";
import { useStore } from "@nanostores/react";
import { $messages, $activeSessionID, $busySessions, $agentError } from "../store";
import { Message } from "./Message";
import { ChatInput } from "./ChatInput";
import { PermissionDialog } from "./PermissionDialog";
import { MessageSquare, Sparkles } from "lucide-react";

export function Chat() {
  const messages = useStore($messages);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const agentError = useStore($agentError);
  const bottomRef = useRef<HTMLDivElement>(null);

  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages, isBusy, agentError]);

  return (
    <div className="flex-1 flex flex-col overflow-hidden relative bg-white">
      <div className="flex-1 overflow-y-auto py-8 flex flex-col">
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
          messages.map((m) => <Message key={m.ID} message={m} />)
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

      <PermissionDialog />
      <ChatInput />
    </div>
  );
}
