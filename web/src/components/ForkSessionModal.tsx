import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { GitFork } from "lucide-react";
import { ws } from "../ws";

interface Props {
  sessionID: string;
  defaultTitle: string;
  onClose: () => void;
}

export function ForkSessionModal({ sessionID, defaultTitle, onClose }: Props) {
  const [title, setTitle] = useState(defaultTitle);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    inputRef.current?.focus();
    inputRef.current?.select();
  }, []);

  function confirm() {
    const t = title.trim() || defaultTitle;
    ws.send("fork_session", { sessionID, title: t });
    onClose();
  }

  function onKey(e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter") confirm();
    if (e.key === "Escape") onClose();
  }

  return createPortal(
    <div
      className="fixed inset-0 z-[9999] flex items-center justify-center bg-black/40 backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        className="chat-font bg-canvas border border-surface rounded-2xl shadow-2xl p-6 w-full max-w-md mx-4"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2.5 mb-5">
          <GitFork size={18} className="text-accent shrink-0" />
          <h2 className="font-bold text-text">Fork session</h2>
        </div>
        <label className="block text-text-muted mb-2 text-sm font-medium">
          Name for the new session
        </label>
        <input
          ref={inputRef}
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          onKeyDown={onKey}
          className="w-full bg-base-overlay border border-surface focus:border-accent/60 rounded-xl px-4 py-2.5 text-text outline-none transition-colors"
          style={{ fontSize: "var(--chat-font-size)" }}
        />
        <div className="flex gap-2 justify-end mt-5">
          <button
            onClick={onClose}
            className="px-4 py-2 rounded-xl bg-base-overlay border border-surface text-text-subtle hover:text-text transition-colors text-sm"
          >
            Cancel
          </button>
          <button
            onClick={confirm}
            className="px-4 py-2 rounded-xl btn-primary text-sm flex items-center gap-1.5"
          >
            <GitFork size={14} />
            Fork
          </button>
        </div>
      </div>
    </div>,
    document.body
  );
}
