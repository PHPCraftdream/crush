import { useState, useRef, useCallback, useEffect } from "react";
import { useStore } from "@nanostores/react";
import {
  $activeSessionID,
  $busySessions,
  $messages,
  enqueueMessage,
  resendLastUserMessage,
  sendWithSmallModel,
} from "../store";
import { ws } from "../ws";
import { ListOrdered, RotateCcw, Send, SendHorizonal } from "lucide-react";

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

  const messages = useStore($messages);
  const hasUserMessage = messages.some((m) => m.Role === "user");

  const send = useCallback(() => {
    const msg = text.trim();
    if (!msg || !activeSessionID) return;
    if (agentBusy) {
      // Queue the message instead of sending immediately
      enqueueMessage(activeSessionID, msg);
    } else {
      ws.send("send_message", { sessionID: activeSessionID, content: msg });
    }
    setText("");
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy]);

  const sendSmall = useCallback(() => {
    const msg = text.trim();
    if (!msg || !activeSessionID || agentBusy) return;
    sendWithSmallModel(activeSessionID, msg);
    setText("");
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy]);

  const rerun = useCallback(() => {
    if (!activeSessionID || agentBusy) return;
    resendLastUserMessage();
  }, [activeSessionID, agentBusy]);

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

  const canSend = !!text.trim() && !!activeSessionID;
  const placeholder = activeSessionID
    ? agentBusy ? "Message… (will be queued)" : "Message… (Enter to send)"
    : "Select or create a session";

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
        <div className="flex items-center gap-2 shrink-0">
          {!agentBusy && hasUserMessage && (
            <button
              onClick={rerun}
              title="Rerun last prompt"
              className="p-2.5 text-text-subtle hover:text-accent hover:bg-accent/10 rounded-xl transition-all active:scale-95"
            >
              <RotateCcw size={18} />
            </button>
          )}
          {!agentBusy && (
            <button
              onClick={sendSmall}
              disabled={!canSend}
              title="Send with lightweight model"
              className="p-2.5 text-text-subtle hover:text-accent hover:bg-accent/10 rounded-xl transition-all active:scale-95 disabled:opacity-30"
            >
              <SendHorizonal size={18} />
            </button>
          )}
          <button
            onClick={send}
            disabled={!canSend}
            className={`font-bold rounded-xl px-5 py-2.5 text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2 ${
              agentBusy
                ? "bg-base-overlay border border-surface text-text-subtle hover:border-accent/50 hover:text-text"
                : "bg-accent text-white hover:bg-accent/90"
            }`}
          >
            {agentBusy ? (
              <>
                <ListOrdered size={15} />
                Queue
              </>
            ) : (
              <>
                <Send size={15} />
                Send
              </>
            )}
          </button>
        </div>
      </div>
      <p className="text-center text-text-subtle text-sm mt-3 font-medium">
        Shift+Enter for newline
      </p>
    </div>
  );
}
