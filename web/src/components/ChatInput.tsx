import { useState, useRef, useCallback, useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $activeSessionID, $busySessions } from "../store";
import { ws } from "../ws";

export function ChatInput() {
  const [text, setText] = useState("");
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (activeSessionID && textareaRef.current) {
      textareaRef.current.focus();
    }
  }, [activeSessionID]);

  const agentBusy = activeSessionID ? busySessions.has(activeSessionID) : false;

  const send = useCallback(() => {
    const msg = text.trim();
    if (!msg || !activeSessionID || agentBusy) return;
    
    // We no longer need to send explicit overrides in every message
    // because the backend now reads them from the session record in DB.
    // The UI stays in sync because Header updates the DB immediately on change.
    
    const payload: Record<string, unknown> = { sessionID: activeSessionID, content: msg };
    
    ws.send("send_message", payload);
    setText("");
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy]);

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
    <div className="px-8 py-6 border-t border-surface bg-white shrink-0">
      <div className={`flex gap-4 items-end bg-base-overlay border rounded-2xl px-5 py-4 transition-all ${
        activeSessionID ? "border-surface focus-within:border-accent/50 focus-within:shadow-md focus-within:bg-white" : "border-surface opacity-60"
      }`}>
        <textarea
          ref={textareaRef}
          value={text}
          onChange={onInput}
          onKeyDown={onKey}
          placeholder={placeholder}
          disabled={!activeSessionID}
          autoFocus
          rows={1}
          className="flex-1 bg-transparent border-none outline-none resize-none text-text text-[16px] leading-relaxed min-h-[28px] max-h-80 overflow-y-auto disabled:cursor-not-allowed placeholder:text-text-subtle font-medium"
        />
        {agentBusy ? (
          <button
            onClick={cancel}
            className="shrink-0 bg-red text-white font-bold rounded-xl px-6 py-2.5 text-base hover:bg-red/90 active:scale-95 transition-all shadow-sm"
          >
            Stop
          </button>
        ) : (
          <button
            onClick={send}
            disabled={!canSend}
            className="shrink-0 bg-accent text-white font-bold rounded-xl px-6 py-2.5 text-base disabled:opacity-30 hover:bg-accent/90 active:scale-95 transition-all shadow-sm"
          >
            Send
          </button>
        )}
      </div>
      <p className="text-center text-text-subtle text-sm mt-3 font-medium">
        Shift+Enter for newline
      </p>
    </div>
  );
}

