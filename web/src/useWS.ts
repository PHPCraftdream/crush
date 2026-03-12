import { useEffect } from "react";
import { ws } from "./ws";
import {
  $connected,
  $config,
  $lspSnapshot,
  $mcpState,
  $agentError,
  $yolo,
  $sessions,
  $activeSessionID,
  $busySessions,
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
  setPermissionRules,
  $recentLargeModels,
  $recentSmallModels,
  trackModelUsage,
  dequeueNextMessage,
  applyTheme,
  setSkills,
  setSummarizeQueued,
} from "./store";
import type { WSMessage, Session, Message, PermissionRequest, ConfigPayload, LSPSnapshot, MCPState, AgentBusyPayload, SkillsSnapshot, SummarizeQueuedPayload } from "./types";

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
        // Reset busy state on reconnect — the replay buffer will re-set any
        // sessions that are truly still running. Without this, a server restart
        // leaves sessions stuck in "busy" forever.
        $busySessions.set(new Set());
        ws.send("list_sessions");
        ws.send("get_config");
        ws.send("get_skills");
        // Sync yolo and theme from localStorage to server on every (re)connect
        // so the server's state always matches what the client has saved locally.
        ws.send("set_yolo", { enabled: $yolo.get() });
        const localTheme = localStorage.getItem("crush_theme");
        if (localTheme) {
          ws.send("set_theme", { theme: localTheme });
        }
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
        // Only process messages for the active session
        if (m.SessionID !== $activeSessionID.get()) return;
        upsertMessage(m);
        if (m.Role === "assistant" && m.Provider && m.Model) {
          trackModelUsage("large", `${m.Provider}:::${m.Model}`);
        }
      }),
      ws.on("message_updated", (msg: WSMessage) => {
        const m = msg.payload as Message;
        // Only process messages for the active session
        if (m.SessionID !== $activeSessionID.get()) return;
        upsertMessage(m);
      }),
      ws.on("message_deleted", (msg: WSMessage) => {
        const m = msg.payload as Message;
        // Only process messages for the active session
        if (m.SessionID !== $activeSessionID.get()) return;
        removeMessage(m.ID);
      }),
      ws.on("messages_list", (msg: WSMessage) =>
        setMessages((msg.payload as Message[]) ?? [])
      ),

      ws.on("permission_request", (msg: WSMessage) => {
        const p = msg.payload as PermissionRequest;
        // Only process permissions for the active session
        if (p.SessionID !== $activeSessionID.get()) return;
        // If yolo is active — auto-grant immediately on the client side without
        // showing the dialog. This is race-free: the server's skip flag might not
        // be set yet when the next request arrives, but we handle it here.
        if ($yolo.get()) {
          ws.send("grant_permission", { permissionID: p.ID });
          return;
        }
        addPermission(p);
      }),
      ws.on("permission_notification", (msg: WSMessage) =>
        removePermission((msg.payload as { ToolCallID: string }).ToolCallID)
      ),

      ws.on("session_permissions", (msg: WSMessage) => {
        const rules = msg.payload as Array<{
          id: string;
          session_id: string;
          tool_name: string;
          action: string;
          path: string;
          created_at: number;
          enabled: number;
        }>;

        if (!rules || rules.length === 0) {
          setPermissionRules($activeSessionID.get() || "", []);
          return;
        }

        // Convert to frontend format
        const permissionRules = rules.map(r => ({
          ID: r.id,
          SessionID: r.session_id,
          ToolName: r.tool_name,
          Action: r.action,
          Path: r.path,
          CreatedAt: r.created_at,
          Enabled: r.enabled !== 0,
        }));

        setPermissionRules($activeSessionID.get() || "", permissionRules);
      }),

      ws.on("config", (msg: WSMessage) => {
        const cfg = msg.payload as ConfigPayload;
        $config.set(cfg);
        // Sync yolo from server only on initial load (localStorage takes priority on reconnect)
        if (cfg.yolo !== undefined && localStorage.getItem("crush_yolo") === null) {
          $yolo.set(cfg.yolo);
        }
        // Apply server theme only if no local preference (first-ever visit).
        // If the user set a theme on the login page, it's already in localStorage
        // and was just pushed to the server above — don't let the echoed config
        // overwrite the local choice.
        if (cfg.theme && !localStorage.getItem("crush_theme")) {
          applyTheme(cfg.theme);
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
        $lspSnapshot.set(msg.payload as LSPSnapshot);
      }),

      ws.on("mcp_state", (msg: WSMessage) =>
        $mcpState.set(msg.payload as MCPState)
      ),

      ws.on("agent_busy", (msg: WSMessage) => {
        const p = msg.payload as AgentBusyPayload;
        setSessionBusy(p.SessionID, p.Busy);
        // When the agent finishes, auto-send the next queued message (if any).
        if (!p.Busy) {
          const next = dequeueNextMessage(p.SessionID);
          if (next) {
            ws.send("send_message", { sessionID: p.SessionID, content: next });
          }
        }
      }),

      ws.on("summarize_queued", (msg: WSMessage) => {
        const p = msg.payload as SummarizeQueuedPayload;
        setSummarizeQueued(p.SessionID, p.Queued);
      }),

      ws.on("skills", (msg: WSMessage) => {
        setSkills((msg.payload as SkillsSnapshot).skills ?? []);
      }),

      ws.on("error", (msg: WSMessage) => {
        $agentError.set((msg.error as string) || "Unknown error");
        setTimeout(() => $agentError.set(null), 8000);
      }),
    ];

    return () => {
      window.removeEventListener("hashchange", onHashChange);
      offs.forEach((off) => off());
      ws.disconnect();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
}
