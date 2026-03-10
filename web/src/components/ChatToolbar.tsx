import { useStore } from "@nanostores/react";
import { Minimize2, Zap, ShieldOff } from "lucide-react";
import {
  $sessions,
  $activeSessionID,
  $busySessions,
  $yolo,
  setYolo,
  summarizeSession,
} from "../store";
import { ModelSelector } from "./ModelSelector";

export function ChatToolbar() {
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const yolo = useStore($yolo);

  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;
  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const hasMessages = (activeSession?.MessageCount ?? 0) > 0;

  if (!activeSessionID) return null;

  return (
    <div className="px-8 pt-3 pb-1 border-t border-surface bg-canvas shrink-0 flex items-center justify-end gap-2 flex-wrap">
      <ModelSelector session={activeSession} modelType="large" />
      <ModelSelector session={activeSession} modelType="small" />

      <div className="w-px h-4 bg-surface/50 mx-1 shrink-0" />

      <button
        onClick={() => summarizeSession(activeSessionID)}
        disabled={isBusy || !hasMessages}
        title="Compact — compress conversation history to free up context window"
        className="btn-toolbar"
      >
        <Minimize2 size={13} />
        Compact
      </button>

      <button
        onClick={() => setYolo(!yolo)}
        title={yolo ? "Yolo ON — all permissions auto-approved" : "Yolo OFF — tool calls require approval"}
        className={`btn-toolbar ${yolo ? "bg-yellow/10 border-yellow/30 text-yellow hover:bg-yellow/20" : ""}`}
      >
        {yolo ? <Zap size={13} /> : <ShieldOff size={13} />}
        Yolo
      </button>
    </div>
  );
}
