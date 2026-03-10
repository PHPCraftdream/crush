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
      className="modal-overlay z-[9999]"
      onClick={onClose}
    >
      <div
        className="modal-panel chat-font p-6 w-full max-w-md mx-4"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2.5 mb-5">
          <GitFork size={18} className="text-accent shrink-0" />
          <h2 className="font-bold text-text">Fork session</h2>
        </div>
        <label className="field-label">
          Name for the new session
        </label>
        <input
          ref={inputRef}
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          onKeyDown={onKey}
          className="field-input"
          style={{ fontSize: "var(--chat-font-size)" }}
        />
        <div className="modal-footer mt-5">
          <button
            onClick={onClose}
            className="btn-cancel text-sm"
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
