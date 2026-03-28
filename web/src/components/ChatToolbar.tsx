import { useStore } from "@nanostores/react";
import { Minimize2, Zap, ShieldOff, X } from "lucide-react";
import {
  $sessions,
  $activeSessionID,
  $busySessions,
  $summarizeQueued,
  $yolo,
  setYolo,
  summarizeSession,
  cancelQueuedSummarize,
} from "../store";
import { ModelSelector } from "./ModelSelector";

export function ChatToolbar() {
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const summarizeQueued = useStore($summarizeQueued);
  const yolo = useStore($yolo);

  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;
  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const isQueued = activeSessionID ? summarizeQueued.has(activeSessionID) : false;
  const hasMessages = (activeSession?.MessageCount ?? 0) > 0;

  if (!activeSessionID) return null;

  return (
    <div className="px-5 pt-3 pb-1 border-t border-surface bg-canvas shrink-0 flex items-center gap-2 flex-wrap">
      <span className="text-text-subtle text-xs font-medium mr-auto">Shift+Enter for newline · Paste or drop files to attach</span>
      <ModelSelector session={activeSession} modelType="large" />
      <ModelSelector session={activeSession} modelType="small" />

      <div className="w-px h-4 bg-surface/50 mx-1 shrink-0" />

      {isQueued ? (
        <button
          onClick={() => cancelQueuedSummarize(activeSessionID)}
          title="Compact is queued — click to cancel"
          className="btn-toolbar text-accent border-accent/30 bg-accent/5 hover:bg-red/10 hover:text-red hover:border-red/30 flex items-center gap-1"
        >
          <Minimize2 size={13} />
          Compact queued
          <X size={11} className="opacity-60" />
        </button>
      ) : (
        <button
          onClick={() => summarizeSession(activeSessionID)}
          disabled={!hasMessages}
          title={isBusy ? "Compact will run after the current task finishes" : "Compact — compress conversation history to free up context window"}
          className="btn-toolbar"
        >
          <Minimize2 size={13} />
          Compact
        </button>
      )}

      <button
        onClick={() => activeSessionID && setYolo(activeSessionID, !yolo)}
        title={yolo ? "Yolo ON — all permissions auto-approved" : "Yolo OFF — tool calls require approval"}
        data-test-id="yolo-button"
        className={`btn-toolbar ${yolo ? "bg-yellow/10 border-yellow/30 text-yellow hover:bg-yellow/20" : ""}`}
      >
        {yolo ? <Zap size={13} /> : <ShieldOff size={13} />}
        Yolo
      </button>

    </div>
  );
}
