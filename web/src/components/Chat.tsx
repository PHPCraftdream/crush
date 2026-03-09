import { useEffect, useRef } from "react";
import { useStore } from "@nanostores/react";
import { $messages, $activeSessionID, $busySessions, $agentError } from "../store";
import { Message } from "./Message";
import { ChatInput } from "./ChatInput";
import { PermissionDialog } from "./PermissionDialog";

export function Chat() {
  const messages = useStore($messages);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const agentError = useStore($agentError);
  const bottomRef = useRef<HTMLDivElement>(null);

  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  return (
    <div className="flex-1 flex flex-col overflow-hidden relative bg-white">
      <div className="flex-1 overflow-y-auto py-6 flex flex-col">
        {!activeSessionID ? (
          <div className="flex flex-col items-center justify-center flex-1 text-center px-8">
            <div className="w-16 h-16 rounded-2xl bg-base-overlay flex items-center justify-center mb-4 text-3xl">
              💬
            </div>
            <p className="text-text-muted font-medium">No session selected</p>
            <p className="text-text-subtle text-sm mt-1">Select a session from the sidebar or create a new one</p>
          </div>
        ) : messages.length === 0 ? (
          <div className="flex flex-col items-center justify-center flex-1 text-center px-8">
            <div className="w-16 h-16 rounded-2xl bg-base-overlay flex items-center justify-center mb-4 text-3xl">
              ✨
            </div>
            <p className="text-text-muted font-medium">No messages yet</p>
            <p className="text-text-subtle text-sm mt-1">Say something to get started</p>
          </div>
        ) : (
          messages.map((m) => <Message key={m.ID} message={m} />)
        )}

        {isBusy && (
          <div className="flex gap-1.5 px-6 py-3 animate-pulse-dots">
            <span className="w-2 h-2 rounded-full bg-accent/60 inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent/60 inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent/60 inline-block" />
          </div>
        )}
        <div ref={bottomRef} />
      </div>

      {agentError && (
        <div className="mx-6 mb-2 px-4 py-2.5 bg-red/10 border border-red/30 rounded-lg text-red text-sm flex items-center justify-between gap-3">
          <span>⚠ {agentError}</span>
          <button onClick={() => $agentError.set(null)} className="text-red/60 hover:text-red transition-colors text-base leading-none shrink-0">✕</button>
        </div>
      )}
      <PermissionDialog />
      <ChatInput />
    </div>
  );
}
