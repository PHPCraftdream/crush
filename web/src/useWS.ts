import { useEffect } from "react";
import { ws } from "./ws";
import {
  $connected,
  $config,
  $mcpState,
  $agentError,
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
  dequeueAllMessages,
  applyTheme,
  setSkills,
  setSummarizeQueued,
  registerSubAgentSession,
  isSubAgentSession,
  getParentSessionID,
  upsertSubAgentMessage,
  setSubAgentMessages,
  trackMessageParts,
} from "./store";
import type { WSMessage, Session, Message, PermissionRequest, ConfigPayload, MCPState, AgentBusyPayload, SkillsSnapshot, SummarizeQueuedPayload } from "./types";
import { isKeepAliveRunning, startKeepAlive, stopKeepAlive, installKeepAliveAutoResume } from "./keepAlive";
import { installSitterAutoRestore } from "./sitter";

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
    // One-shot per session: re-resume the AudioContext when the tab is
    // foregrounded after a long sleep. The actual start/stop is gated on
    // the server-side preference (see the `config` event handler below).
    installKeepAliveAutoResume();
    // Re-arm the sitter loop if the operator had it on before reload.
    // First tick happens after the saved interval — no risk of a flurry
    // of resume messages on every reload.
    installSitterAutoRestore();

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
        // Sync theme from localStorage to server on every (re)connect
        // so the server's state always matches what the client has saved locally.
        const localTheme = localStorage.getItem("crush_theme");
        if (localTheme) {
          ws.send("set_theme", { theme: localTheme });
        }
      }),

      ws.on("_disconnected", () => {
        $connected.set(false);
        // Stop the BT keep-alive noise loop while the backend is
        // unreachable — no point holding the audio device awake for a
        // session that isn't running. It comes back automatically once
        // the backend reconnects and re-sends `config` (see the "config"
        // handler below), which re-applies the user's preference.
        if (isKeepAliveRunning()) stopKeepAlive();
      }),

      ws.on("session_created", (msg: WSMessage) => {
        const s = msg.payload as Session;
        console.log("[useWS] session_created:", s.ID, "ParentSessionID:", s.ParentSessionID);
        upsertSession(s);
        if (s.ParentSessionID) {
          registerSubAgentSession(s.ID, s.ParentSessionID);
          ws.send("load_messages", { sessionID: s.ID });
          return;
        }
        setActiveSession(s.ID);
        ws.send("load_messages", { sessionID: s.ID });
      }),
      ws.on("session_updated", (msg: WSMessage) => {
        const session = msg.payload as Session;
        upsertSession(session);
      }),
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

        // Foreign-owned active session: kick a load_messages refresh on
        // every sessions_list poll too. This guarantees we never sit
        // longer than the sessions poll interval (5s) without a fresh
        // history read, in case the dedicated 1.5s messages poll
        // missed a window during a pause.
        const activeID0 = $activeSessionID.get();
        if (activeID0) {
          const a = sessions.find((s) => s.ID === activeID0);
          if (a && a.OwnedExternal) {
            ws.send("load_messages", { sessionID: activeID0 });
          }
        }

        for (const s of sessions) {
          if (s.ParentSessionID) {
            registerSubAgentSession(s.ID, s.ParentSessionID);
          }
        }

        const topLevelSessions = sessions.filter((s) => !s.ParentSessionID);
        if (topLevelSessions.length === 0) {
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

        // If no valid hash or session not found, pick the most recent non-sub-agent session
        const latest = sessions.find((s) => !s.ParentSessionID);
        if (latest && activeID !== latest.ID) {
          setActiveSession(latest.ID);
          ws.send("load_messages", { sessionID: latest.ID });
        }
      }),

      ws.on("message_created", (msg: WSMessage) => {
        const m = msg.payload as Message;
        if (isSubAgentSession(m.SessionID)) {
          upsertSubAgentMessage(m.SessionID, m);
          return;
        }
        const activeID = $activeSessionID.get();
        if (!activeID || m.SessionID !== activeID) return;
        upsertMessage(m);
        if (m.Role === "assistant" && m.Provider && m.Model) {
          trackModelUsage("large", `${m.Provider}:::${m.Model}`);
        }
      }),
      ws.on("message_updated", (msg: WSMessage) => {
        const m = msg.payload as Message;
        if (isSubAgentSession(m.SessionID)) {
          upsertSubAgentMessage(m.SessionID, m);
          return;
        }
        const activeID = $activeSessionID.get();
        if (!activeID || m.SessionID !== activeID) return;
        if (m.Role === "assistant") {
          trackMessageParts(m.ID, m.Parts);
        }
        upsertMessage(m);
      }),
      ws.on("message_deleted", (msg: WSMessage) => {
        const m = msg.payload as Message;
        // Only process messages for the active session
        const activeID = $activeSessionID.get();
        if (!activeID || m.SessionID !== activeID) return;
        removeMessage(m.ID);
      }),
      ws.on("messages_list", (msg: WSMessage) => {
        // New envelope: { SessionID, Messages }. Old shape (raw array) is
        // kept as a fallback for back-compat with cached frontends, but the
        // backend now always wraps so we can route empty replies safely —
        // a lazy load_messages for an empty sub-agent session must NOT
        // overwrite the active main session's messages.
        const payload = msg.payload as
          | { SessionID?: string; Messages?: Message[] }
          | Message[]
          | undefined;
        let sid: string | undefined;
        let msgs: Message[] = [];
        if (Array.isArray(payload)) {
          msgs = payload;
          sid = msgs[0]?.SessionID;
        } else if (payload) {
          msgs = payload.Messages ?? [];
          sid = payload.SessionID ?? msgs[0]?.SessionID;
        }
        if (sid && isSubAgentSession(sid)) {
          setSubAgentMessages(sid, msgs);
          return;
        }
        // For the main chat: only apply if it's for the currently active
        // session (we might have polled in flight and the user switched).
        const activeID = $activeSessionID.get();
        if (sid && activeID && sid !== activeID) return;
        setMessages(msgs);
      }),

      ws.on("permission_request", (msg: WSMessage) => {
        const p = msg.payload as PermissionRequest;
        // Only process permissions for the active session
        if (p.SessionID !== $activeSessionID.get()) return;
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
        // Apply theme from server (backend is source of truth)
        if (cfg.theme) {
          applyTheme(cfg.theme);
        }
        // Sync the WebAudio keep-alive runtime to the server preference.
        // Backend resolves nil → true, so we treat anything other than an
        // explicit false as "ON". AudioContext requires a user gesture;
        // if startKeepAlive runs before the user has clicked anything in
        // the page, the AudioContext is constructed in suspended state —
        // installKeepAliveAutoResume + the visibilitychange listener
        // handle the resume once a gesture lands.
        const wantOn = cfg.keepAliveEnabled !== false;
        if (wantOn && !isKeepAliveRunning()) startKeepAlive();
        else if (!wantOn && isKeepAliveRunning()) stopKeepAlive();
        // Restore recent models from server (persisted across restarts)
        if (cfg.recentLargeModels?.length) {
          const keys = cfg.recentLargeModels.map(m => `${m.Provider}:::${m.Model}`);
          $recentLargeModels.set(keys);
        } else {
          $recentLargeModels.set([]);
        }
        if (cfg.recentSmallModels?.length) {
          const keys = cfg.recentSmallModels.map(m => `${m.Provider}:::${m.Model}`);
          $recentSmallModels.set(keys);
        } else {
          $recentSmallModels.set([]);
        }
      }),

      ws.on("mcp_state", (msg: WSMessage) =>
        $mcpState.set(msg.payload as MCPState)
      ),

      ws.on("agent_busy", (msg: WSMessage) => {
        const p = msg.payload as AgentBusyPayload;
        setSessionBusy(p.SessionID, p.Busy);
        // When the agent finishes, send all queued messages as one combined message.
        if (!p.Busy) {
          const combined = dequeueAllMessages(p.SessionID);
          if (combined) {
            ws.send("send_message", { sessionID: p.SessionID, content: combined });
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

    // Visibility-gated polling. When the tab is hidden we let the WS
    // pubsub do its thing without any extra requests. When the tab is
    // visible:
    //   - poll sessions_list every 5s — keeps the sidebar fresh (titles,
    //     ownership, message counts) even when another crush process
    //     drives a session on the same .crush/.
    //   - if the active session is externally owned (another process
    //     holds the lock — OwnedExternal: true), poll its messages_list
    //     every 1.5s so the conversation streams visibly without going
    //     through that other process's in-memory pubsub.
    // On visibilitychange we tear down both intervals together, then
    // rebuild them and do an immediate fire when the tab comes back.
    let listInterval: number | undefined;
    let messagesInterval: number | undefined;

    const SESSIONS_POLL_MS = 5000;
    const FOLLOW_MESSAGES_POLL_MS = 1500;

    const stopPolling = () => {
      if (listInterval !== undefined) { clearInterval(listInterval); listInterval = undefined; }
      if (messagesInterval !== undefined) { clearInterval(messagesInterval); messagesInterval = undefined; }
    };

    const pollMessagesIfFollowed = () => {
      const id = $activeSessionID.get();
      if (!id) return;
      const sess = $sessions.get().find((s) => s.ID === id);
      if (!sess || !sess.OwnedExternal) return;
      ws.send("load_messages", { sessionID: id });
    };

    const startPolling = () => {
      stopPolling();
      // Immediate refresh on tab focus so the user doesn't sit through
      // a full interval before the first update lands.
      ws.send("list_sessions");
      pollMessagesIfFollowed();
      listInterval = window.setInterval(() => ws.send("list_sessions"), SESSIONS_POLL_MS);
      messagesInterval = window.setInterval(pollMessagesIfFollowed, FOLLOW_MESSAGES_POLL_MS);
    };

    const onVisibility = () => {
      if (document.visibilityState === "visible") startPolling();
      else stopPolling();
    };

    document.addEventListener("visibilitychange", onVisibility);
    if (document.visibilityState === "visible") startPolling();

    return () => {
      window.removeEventListener("hashchange", onHashChange);
      document.removeEventListener("visibilitychange", onVisibility);
      stopPolling();
      offs.forEach((off) => off());
      ws.disconnect();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
}
