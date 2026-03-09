import { atom } from "nanostores";
import type { Session, Message, PermissionRequest, ConfigPayload, LSPState, MCPState } from "./types";

// ── Connection state ─────────────────────────────────────────────────────────
export const $connected = atom(false);
export const $authed = atom(false);

// ── Data ─────────────────────────────────────────────────────────────────────
export const $sessions = atom<Session[]>([]);
export const $activeSessionID = atom<string | null>(null);
export const $messages = atom<Message[]>([]);
export const $permissions = atom<PermissionRequest[]>([]);
export const $config = atom<ConfigPayload | null>(null);
export const $lspStates = atom<LSPState[]>([]);
export const $mcpState = atom<MCPState | null>(null);
export const $busySessions = atom<Set<string>>(new Set());

// ── Actions ──────────────────────────────────────────────────────────────────
export function setSessions(sessions: Session[]) {
  $sessions.set(sessions);
}

// ── Model History ────────────────────────────────────────────────────────────
const STORAGE_KEY_RECENT_LARGE = "crush_recent_models_large";
const STORAGE_KEY_RECENT_SMALL = "crush_recent_models_small";

function loadRecent(key: string): string[] {
  try {
    const val = localStorage.getItem(key);
    return val ? JSON.parse(val) : [];
  } catch {
    return [];
  }
}

export const $recentLargeModels = atom<string[]>(loadRecent(STORAGE_KEY_RECENT_LARGE));
export const $recentSmallModels = atom<string[]>(loadRecent(STORAGE_KEY_RECENT_SMALL));

export function trackModelUsage(role: "large" | "small", modelKey: string) {
  const store = role === "large" ? $recentLargeModels : $recentSmallModels;
  const storageKey = role === "large" ? STORAGE_KEY_RECENT_LARGE : STORAGE_KEY_RECENT_SMALL;

  const current = store.get();
  const next = [modelKey, ...current.filter((k) => k !== modelKey)].slice(0, 5);

  store.set(next);
  localStorage.setItem(storageKey, JSON.stringify(next));
}

export function removeRecentModel(role: "large" | "small", modelKey: string) {
  const store = role === "large" ? $recentLargeModels : $recentSmallModels;
  const storageKey = role === "large" ? STORAGE_KEY_RECENT_LARGE : STORAGE_KEY_RECENT_SMALL;

  const next = store.get().filter((k) => k !== modelKey);
  store.set(next);
  localStorage.setItem(storageKey, JSON.stringify(next));

  // Persist removal to server so it survives restarts
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("remove_recent_model", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}

export function upsertSession(s: Session) {
  const list = $sessions.get();
  const idx = list.findIndex((x) => x.ID === s.ID);
  if (idx === -1) {
    $sessions.set([s, ...list]);
  } else {
    const next = [...list];
    next[idx] = s;
    $sessions.set(next);
  }
}

export function removeSession(id: string) {
  $sessions.set($sessions.get().filter((s) => s.ID !== id));
}

export function setActiveSession(id: string | null) {
  $activeSessionID.set(id);
  $messages.set([]);
  $agentError.set(null);
  if (id) {
    window.location.hash = `#/${id}`;
  } else {
    window.location.hash = "";
  }
}

export function setMessages(msgs: Message[]) {
  $messages.set(msgs);
}

export function upsertMessage(msg: Message) {
  const list = $messages.get();
  const idx = list.findIndex((m) => m.ID === msg.ID);
  if (idx === -1) {
    $messages.set([...list, msg]);
  } else {
    const next = [...list];
    next[idx] = msg;
    $messages.set(next);
  }
}

export function removeMessage(id: string) {
  $messages.set($messages.get().filter((m) => m.ID !== id));
}

export function addPermission(p: PermissionRequest) {
  $permissions.set([...$permissions.get(), p]);
}

export function removePermission(toolCallID: string) {
  $permissions.set(
    $permissions.get().filter((p) => p.ToolCallID !== toolCallID)
  );
}

export function setSessionBusy(sessionID: string, busy: boolean) {
  const s = new Set($busySessions.get());
  if (busy) s.add(sessionID);
  else s.delete(sessionID);
  $busySessions.set(s);
}

// Per-session model overrides: removed in favor of global selection
// Now using the session object from DB as source of truth.

export function getDefaultModelKey(role: "large" | "small", config: ConfigPayload | null): string {
  const entry = config?.models?.[role];
  if (entry) return `${entry.Provider}:::${entry.Model}`;
  return "";
}

import { ws } from "./ws";

export function setSessionModels(sessionID: string, largeKey: string | null, smallKey: string | null) {
  const parse = (key: string | null) => {
    if (!key) return null;
    const idx = key.indexOf(":::");
    if (idx === -1) return null;
    return { provider: key.slice(0, idx), model: key.slice(idx + 3) };
  };

  ws.send("set_session_models", {
    sessionID,
    largeModel: parse(largeKey),
    smallModel: parse(smallKey),
  });
}

// ── Theme ─────────────────────────────────────────────────────────────────────

export function applyTheme(theme: string) {
  if (theme === "dark") {
    document.documentElement.classList.add("dark");
  } else {
    document.documentElement.classList.remove("dark");
  }
}

export function setTheme(theme: "light" | "dark") {
  applyTheme(theme);
  ws.send("set_theme", { theme });
}

// ── Yolo mode ────────────────────────────────────────────────────────────────
const STORAGE_KEY_YOLO = "crush_yolo";

function loadYolo(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY_YOLO) === "true";
  } catch {
    return false;
  }
}

export const $yolo = atom<boolean>(loadYolo());

export function setYolo(enabled: boolean) {
  $yolo.set(enabled);
  localStorage.setItem(STORAGE_KEY_YOLO, String(enabled));
  ws.send("set_yolo", { enabled });
}

export function setProviderKey(providerID: string, apiKey: string) {
  ws.send("set_provider_key", { providerID, apiKey });
}

export function removeProviderKey(providerID: string) {
  ws.send("remove_provider_key", { providerID });
}

export function deleteMessage(messageID: string) {
  ws.send("delete_message", { messageID });
}

export function deleteMessages(messageIDs: string[]) {
  ws.send("delete_messages", { messageIDs });
}

export function updateMessageContent(messageID: string, content: string) {
  ws.send("update_message_content", { messageID, content });
}

export function updateMessageThinking(messageID: string, thinking: string) {
  ws.send("update_message_thinking", { messageID, thinking });
}

export function summarizeSession(sessionID: string) {
  ws.send("summarize_session", { sessionID });
}

export function deleteMessagePart(messageID: string, partIndex: number) {
  ws.send("delete_message_part", { messageID, partIndex });
}

export function updateMessagePart(messageID: string, partIndex: number, content: string) {
  ws.send("update_message_part", { messageID, partIndex, content });
}

export function rerunFromMessage(messageID: string) {
  const msgs = $messages.get();
  const sessionID = $activeSessionID.get();
  if (!sessionID) return;

  const idx = msgs.findIndex((m) => m.ID === messageID);
  if (idx === -1) return;

  const text = msgs[idx].Parts.filter((p) => p.type === "text")
    .map((p) => (p as { type: "text"; Text: string }).Text)
    .join("\n")
    .trim();
  if (!text) return;

  // Delete all messages that came after this user message (agent responses)
  const toDelete = msgs.slice(idx + 1).map((m) => m.ID);
  if (toDelete.length > 0) {
    deleteMessages(toDelete);
  }

  ws.send("send_message", { sessionID, content: text });
}

export function sendWithSmallModel(sessionID: string, content: string) {
  const config = $config.get();
  const sess = $sessions.get().find((s) => s.ID === sessionID);
  let smallModel: { provider: string; model: string } | undefined;
  if (sess && sess.SmallModelID) {
    smallModel = { provider: sess.SmallModelProvider, model: sess.SmallModelID };
  } else if (config?.models?.small) {
    smallModel = { provider: config.models.small.Provider, model: config.models.small.Model };
  }
  const payload: Record<string, unknown> = { sessionID, content };
  if (smallModel) {
    payload.largeModel = smallModel;
  }
  ws.send("send_message", payload);
}

// ── Batch selection ───────────────────────────────────────────────────────────
export const $selectedMessageIDs = atom<Set<string>>(new Set());

export function toggleMessageSelection(id: string) {
  const s = new Set($selectedMessageIDs.get());
  if (s.has(id)) s.delete(id); else s.add(id);
  $selectedMessageIDs.set(s);
}

export function clearSelection() {
  $selectedMessageIDs.set(new Set());
}

// Last agent error to display in chat
export const $agentError = atom<string | null>(null);

// ── Message queue (client-side) ────────────────────────────────────────────────
export interface QueuedMessage {
  id: string;
  content: string;
}

export const $messageQueue = atom<Map<string, QueuedMessage[]>>(new Map());

let _queueIDCounter = 0;
function newQueueID() { return `q-${++_queueIDCounter}`; }

export function enqueueMessage(sessionID: string, content: string) {
  const q = new Map($messageQueue.get());
  q.set(sessionID, [...(q.get(sessionID) ?? []), { id: newQueueID(), content }]);
  $messageQueue.set(q);
}

export function dequeueNextMessage(sessionID: string): string | undefined {
  const q = new Map($messageQueue.get());
  const msgs = q.get(sessionID) ?? [];
  if (!msgs.length) return undefined;
  const [first, ...rest] = msgs;
  if (!rest.length) q.delete(sessionID); else q.set(sessionID, rest);
  $messageQueue.set(q);
  return first.content;
}

export function removeQueuedMessage(sessionID: string, id: string) {
  const q = new Map($messageQueue.get());
  const msgs = (q.get(sessionID) ?? []).filter((m) => m.id !== id);
  if (!msgs.length) q.delete(sessionID); else q.set(sessionID, msgs);
  $messageQueue.set(q);
}

export function updateQueuedMessage(sessionID: string, id: string, content: string) {
  const q = new Map($messageQueue.get());
  const msgs = (q.get(sessionID) ?? []).map((m) => m.id === id ? { ...m, content } : m);
  q.set(sessionID, msgs);
  $messageQueue.set(q);
}
