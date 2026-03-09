import { useEffect } from "react";
import { ws } from "./ws";
import {
  $connected,
  $config,
  $lspStates,
  $mcpState,
  $agentError,
  setSessions,
  upsertSession,
  removeSession,
  setMessages,
  upsertMessage,
  removeMessage,
  addPermission,
  removePermission,
  setSessionBusy,
} from "./store";
import type { WSMessage, Session, Message, PermissionRequest, ConfigPayload, LSPState, MCPState, AgentBusyPayload } from "./types";

export function useWS() {
  useEffect(() => {
    ws.connect();

    const offs = [
      ws.on("_connected", () => {
        $connected.set(true);
        ws.send("list_sessions");
        ws.send("get_config");
      }),

      ws.on("_disconnected", () => {
        $connected.set(false);
      }),

      ws.on("session_created", (msg: WSMessage) =>
        upsertSession(msg.payload as Session)
      ),
      ws.on("session_updated", (msg: WSMessage) =>
        upsertSession(msg.payload as Session)
      ),
      ws.on("session_deleted", (msg: WSMessage) =>
        removeSession((msg.payload as { ID: string }).ID)
      ),
      ws.on("sessions_list", (msg: WSMessage) =>
        setSessions((msg.payload as Session[]) ?? [])
      ),

      ws.on("message_created", (msg: WSMessage) =>
        upsertMessage(msg.payload as Message)
      ),
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

      ws.on("config", (msg: WSMessage) =>
        $config.set(msg.payload as ConfigPayload)
      ),

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
        // Auto-clear after 8 seconds
        setTimeout(() => $agentError.set(null), 8000);
      }),
    ];

    return () => {
      offs.forEach((off) => off());
      ws.disconnect();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
}
