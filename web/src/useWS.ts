import { useEffect } from "react";
import { ws } from "./ws";
import {
  $connected,
  $config,
  $lspStates,
  $mcpState,
  $agentError,
  $yolo,
  $sessions,
  $activeSessionID,
  setSessions,
  upsertSession,
  removeSession,
  setMessages,
  upsertMessage,
  removeMessage,
  addPermission,
  removePermission,
  setSessionBusy,
  setActiveSession,
  trackModelUsage,
  $recentLargeModels,
  $recentSmallModels,
} from "./store";
import type { WSMessage, Session, Message, PermissionRequest, ConfigPayload, LSPState, MCPState, AgentBusyPayload } from "./types";

function getIDFromHash(): string | null {
  const hash = window.location.hash; // #/uuid
  if (hash.startsWith("#/")) {
    return hash.slice(2);
  }
  return null;
}

export function useWS() {
  useEffect(() => {
    ws.connect();

    const onHashChange = () => {
      const id = getIDFromHash();
      const currentActive = $activeSessionID.get();
      if (id && id !== currentActive) {
        const sessionExists = $sessions.get().some((s) => s.ID === id);
        if (sessionExists) {
          setActiveSession(id);
          ws.send("load_messages", { sessionID: id });
        }
      } else if (!id && currentActive) {
        setActiveSession(null);
      }
    };
    window.addEventListener("hashchange", onHashChange);

    const offs = [
      ws.on("_connected", () => {
        $connected.set(true);
        ws.send("list_sessions");
        ws.send("get_config");
      }),

      ws.on("_disconnected", () => {
        $connected.set(false);
      }),

      ws.on("session_created", (msg: WSMessage) => {
        const s = msg.payload as Session;
        upsertSession(s);
        // Always select newly created session (whether auto-created or manual)
        setActiveSession(s.ID);
        ws.send("load_messages", { sessionID: s.ID });
      }),
      ws.on("session_updated", (msg: WSMessage) =>
        upsertSession(msg.payload as Session)
      ),
      ws.on("session_deleted", (msg: WSMessage) => {
        const id = (msg.payload as { ID: string }).ID;
        removeSession(id);
        if ($activeSessionID.get() === id) {
          setActiveSession(null);
        }
      }),
      ws.on("sessions_list", (msg: WSMessage) => {
        const sessions = (msg.payload as Session[]) ?? [];
        setSessions(sessions);

        if (sessions.length === 0) {
          // No sessions? Auto-create one.
          ws.send("create_session");
          return;
        }

        const hashID = getIDFromHash();
        const activeID = $activeSessionID.get();

        if (hashID) {
          const session = sessions.find((s) => s.ID === hashID);
          if (session) {
            if (activeID !== hashID) {
              setActiveSession(hashID);
              ws.send("load_messages", { sessionID: hashID });
            }
            return;
          }
        }

        // If no valid hash or session not found, pick the most recent one (first in list)
        const latest = sessions[0];
        if (latest && activeID !== latest.ID) {
          setActiveSession(latest.ID);
          ws.send("load_messages", { sessionID: latest.ID });
        }
      }),

      ws.on("message_created", (msg: WSMessage) => {
        const m = msg.payload as Message;
        upsertMessage(m);
        // Track model as recently used when assistant responds successfully
        if (m.Role === "assistant" && m.Provider && m.Model) {
          trackModelUsage("large", `${m.Provider}:::${m.Model}`);
        }
      }),
      ws.on("message_updated", (msg: WSMessage) =>
        upsertMessage(msg.payload as Message)
      ),
      ws.on("message_deleted", (msg: WSMessage) =>
        removeMessage((msg.payload as { ID: string }).ID)
      ),
      ws.on("messages_list", (msg: WSMessage) =>
        setMessages((msg.payload as Message[]) ?? [])
      ),

      ws.on("permission_request", (msg: WSMessage) =>
        addPermission(msg.payload as PermissionRequest)
      ),
      ws.on("permission_notification", (msg: WSMessage) =>
        removePermission((msg.payload as { ToolCallID: string }).ToolCallID)
      ),

      ws.on("config", (msg: WSMessage) => {
        const cfg = msg.payload as ConfigPayload;
        $config.set(cfg);
        // Sync yolo from server only on initial load (localStorage takes priority on reconnect)
        if (cfg.yolo !== undefined && localStorage.getItem("crush_yolo") === null) {
          $yolo.set(cfg.yolo);
        }
        // Restore recent models from server (persisted across restarts)
        if (cfg.recentLargeModels?.length) {
          const keys = cfg.recentLargeModels.map(m => `${m.Provider}:::${m.Model}`);
          $recentLargeModels.set(keys);
        }
        if (cfg.recentSmallModels?.length) {
          const keys = cfg.recentSmallModels.map(m => `${m.Provider}:::${m.Model}`);
          $recentSmallModels.set(keys);
        }
      }),

      ws.on("lsp_state", (msg: WSMessage) => {
        const incoming = msg.payload as LSPState;
        const current = $lspStates.get();
        const idx = current.findIndex((l) => l.name === incoming.name);
        if (idx === -1) {
          $lspStates.set([...current, incoming]);
        } else {
          const next = [...current];
          next[idx] = incoming;
          $lspStates.set(next);
        }
      }),

      ws.on("mcp_state", (msg: WSMessage) =>
        $mcpState.set(msg.payload as MCPState)
      ),

      ws.on("agent_busy", (msg: WSMessage) => {
        const p = msg.payload as AgentBusyPayload;
        setSessionBusy(p.SessionID, p.Busy);
      }),

      ws.on("error", (msg: WSMessage) => {
        $agentError.set((msg.error as string) || "Unknown error");
      }),
    ];

    return () => {
      window.removeEventListener("hashchange", onHashChange);
      offs.forEach((off) => off());
      ws.disconnect();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
}
