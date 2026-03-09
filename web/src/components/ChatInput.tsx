import { useState, useRef, useCallback } from "react";
import { useStore } from "@nanostores/react";
import { $activeSessionID, $busySessions, $sessionLargeModel, $sessionSmallModel } from "../store";
import { ws } from "../ws";

// Parses "providerID:::modelID" key into {provider, model} object for the wire format.
function parseModelKey(key: string | undefined): { provider: string; model: string } | undefined {
  if (!key) return undefined;
  const idx = key.indexOf(":::");
  if (idx === -1) return undefined;
  return { provider: key.slice(0, idx), model: key.slice(idx + 3) };
}

export function ChatInput() {
  const [text, setText] = useState("");
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const sessionLargeModels = useStore($sessionLargeModel);
  const sessionSmallModels = useStore($sessionSmallModel);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  const agentBusy = activeSessionID ? busySessions.has(activeSessionID) : false;

  const send = useCallback(() => {
    const msg = text.trim();
    if (!msg || !activeSessionID || agentBusy) return;
    const largeKey = sessionLargeModels[activeSessionID];
    const smallKey = sessionSmallModels[activeSessionID];
    const payload: Record<string, unknown> = { sessionID: activeSessionID, content: msg };
    const largeOverride = parseModelKey(largeKey);
    const smallOverride = parseModelKey(smallKey);
    if (largeOverride) payload.largeModel = largeOverride;
    if (smallOverride) payload.smallModel = smallOverride;
    ws.send("send_message", payload);
    setText("");
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy, sessionLargeModels, sessionSmallModels]);

  function onKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey) {
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

  function cancel() {
    if (activeSessionID) ws.send("cancel_agent", { sessionID: activeSessionID });
  }

  const canSend = !!text.trim() && !!activeSessionID && !agentBusy;
  const placeholder = activeSessionID ? "Message… (Enter to send)" : "Select or create a session";

  return (
    <div className="px-6 py-4 border-t border-surface bg-white shrink-0">
      <div className={`flex gap-3 items-end bg-base-overlay border rounded-xl px-4 py-3 transition-colors ${
        activeSessionID ? "border-surface focus-within:border-accent/50 focus-within:shadow-sm" : "border-surface opacity-60"
      }`}>
        <textarea
          ref={textareaRef}
          value={text}
          onChange={onInput}
          onKeyDown={onKey}
          placeholder={placeholder}
          disabled={!activeSessionID}
          rows={1}
          className="flex-1 bg-transparent border-none outline-none resize-none text-text text-sm leading-6 min-h-[24px] max-h-60 overflow-y-auto disabled:cursor-not-allowed placeholder:text-text-subtle"
        />
        {agentBusy ? (
          <button
            onClick={cancel}
            className="shrink-0 bg-red text-white font-semibold rounded-lg px-4 py-2 text-sm hover:bg-red/90 active:scale-95 transition-all"
          >
            Stop
          </button>
        ) : (
          <button
            onClick={send}
            disabled={!canSend}
            className="shrink-0 bg-accent text-white font-semibold rounded-lg px-4 py-2 text-sm disabled:opacity-30 hover:bg-accent/90 active:scale-95 transition-all"
          >
            Send
          </button>
        )}
      </div>
      <p className="text-center text-text-subtle text-xs mt-2">
        Shift+Enter for newline
      </p>
    </div>
  );
}
