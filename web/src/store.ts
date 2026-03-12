import { atom } from "nanostores";
import type { Session, Message, PermissionRequest, ConfigPayload, LSPSnapshot, MCPState, Todo, SkillInfo } from "./types";

// ── Connection state ─────────────────────────────────────────────────────────
export const $connected = atom(false);
export const $authed = atom(false);

// ── Data ─────────────────────────────────────────────────────────────────────
export const $skills = atom<SkillInfo[]>([]);
export const $sessions = atom<Session[]>([]);
export const $activeSessionID = atom<string | null>(null);
export const $messages = atom<Message[]>([]);
export const $permissions = atom<PermissionRequest[]>([]);
export const $permissionRules = atom<Map<string, PermissionRule[]>>(new Map()); // sessionID -> rules
export const $config = atom<ConfigPayload | null>(null);
export const $lspSnapshot = atom<LSPSnapshot | null>(null);
export const $mcpState = atom<MCPState | null>(null);
export const $busySessions = atom<Set<string>>(new Set());
// Sessions where the user has queued a compact/summarise request.
export const $summarizeQueued = atom<Set<string>>(new Set());

// ── Actions ──────────────────────────────────────────────────────────────────
export function setSkills(skills: SkillInfo[]) {
  $skills.set(skills);
}

const LAST_SKILL_KEY = "crush_last_skill";
export const $lastUsedSkill = atom<string>(localStorage.getItem(LAST_SKILL_KEY) ?? "");
export function setLastUsedSkill(name: string) {
  $lastUsedSkill.set(name);
  localStorage.setItem(LAST_SKILL_KEY, name);
}

export function setSessions(sessions: Session[]) {
  $sessions.set(sessions);
}

// ── Model History ────────────────────────────────────────────────────────────
// Recent models are stored on the backend and synced via WebSocket config.
// Local state is initialized empty and populated when config arrives.
export const $recentLargeModels = atom<string[]>([]);
export const $recentSmallModels = atom<string[]>([]);

export function trackModelUsage(role: "large" | "small", modelKey: string) {
  const store = role === "large" ? $recentLargeModels : $recentSmallModels;

  const current = store.get();
  const next = [modelKey, ...current.filter((k) => k !== modelKey)].slice(0, 5);

  store.set(next);

  // Sync to backend - parse modelKey into provider and model
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("track_model_usage", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}

export function removeRecentModel(role: "large" | "small", modelKey: string) {
  const store = role === "large" ? $recentLargeModels : $recentSmallModels;

  const next = store.get().filter((k) => k !== modelKey);
  store.set(next);

  // Persist removal to server
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
  $permissions.set([]);  // Clear permission dialogs when switching sessions
  $agentError.set(null);
  // Restore YOLO state for the new active session
  if (id) {
    const sessions = $sessions.get();
    const session = sessions.find((s) => s.ID === id);
    const yoloEnabled = session?.YoloEnabled ?? false;
    $yolo.set(yoloEnabled);
    console.log("[setActiveSession] id:", id, "YoloEnabled:", yoloEnabled, "session:", session);
    window.location.hash = `#/${id}`;
  } else {
    $yolo.set(false);
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

// ── Permission Rules (persistent permissions) ─────────────────────────────

export function setPermissionRules(sessionID: string, rules: PermissionRule[]) {
  const map = new Map($permissionRules.get());
  map.set(sessionID, rules);
  $permissionRules.set(map);
}

export function getPermissionRules(sessionID: string): PermissionRule[] {
  return $permissionRules.get().get(sessionID) || [];
}

export function togglePermissionRule(sessionID: string, ruleID: string) {
  const map = new Map($permissionRules.get());
  const rules = (map.get(sessionID) || []).map((r) =>
    r.ID === ruleID ? { ...r, Enabled: !r.Enabled } : r
  );
  map.set(sessionID, rules);
  $permissionRules.set(map);

  // Sync to backend
  const rule = rules.find((r) => r.ID === ruleID);
  if (rule) {
    ws.send("update_permission_rule", { ruleID, enabled: rule.Enabled });
  }
}

export function deletePermissionRule(sessionID: string, ruleID: string) {
  const map = new Map($permissionRules.get());
  const rules = (map.get(sessionID) || []).filter((r) => r.ID !== ruleID);
  map.set(sessionID, rules);
  $permissionRules.set(map);

  // Sync to backend
  ws.send("delete_permission_rule", { ruleID });
}

export function fetchPermissionRules(sessionID: string) {
  ws.send("list_session_permissions", { sessionID });
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
import { logClientEvent } from "./telemetry";

export function updateTodos(sessionID: string, todos: Todo[]) {
  // Optimistic local update
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = { ...next[idx], Todos: todos };
    $sessions.set(next);
  }
  logClientEvent("update_todos", { sessionID, count: todos.length, todos: todos.map((t) => ({ content: t.content, status: t.status })) });
  ws.send("update_todos", { sessionID, todos });
}

export function setSessionModels(sessionID: string, largeKey: string | null, smallKey: string | null) {
  const parse = (key: string | null) => {
    if (!key) return null;
    const idx = key.indexOf(":::");
    if (idx === -1) return null;
    return { provider: key.slice(0, idx), model: key.slice(idx + 3) };
  };

  const large = parse(largeKey);
  const small = parse(smallKey);

  // Optimistic local update so the UI reflects the change immediately
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = {
      ...next[idx],
      ...(large ? { LargeModelProvider: large.provider, LargeModelID: large.model } : {}),
      ...(small ? { SmallModelProvider: small.provider, SmallModelID: small.model } : {}),
    };
    $sessions.set(next);
  }

  ws.send("set_session_models", {
    sessionID,
    largeModel: large,
    smallModel: small,
  });
}

// ── Theme ─────────────────────────────────────────────────────────────────────
// Theme is stored on the backend. localStorage is only used as a cache for
// instant page load (before WS connects) to avoid white flash.

const STORAGE_KEY_THEME = "crush_theme";

export function applyTheme(theme: string) {
  if (theme === "dark") {
    document.documentElement.classList.add("dark");
  } else {
    document.documentElement.classList.remove("dark");
  }
  // Cache to localStorage for instant load on next page refresh
  try { localStorage.setItem(STORAGE_KEY_THEME, theme); } catch {}
}

// Apply cached theme immediately on module load (before WS connects)
// This prevents white flash while waiting for backend connection.
;(function () {
  try {
    const cached = localStorage.getItem(STORAGE_KEY_THEME);
    if (cached) applyTheme(cached);
  } catch {}
})();

export function setTheme(theme: "light" | "dark") {
  applyTheme(theme);
  // Send to backend as source of truth
  ws.send("set_theme", { theme });
}

// ── Yolo mode ────────────────────────────────────────────────────────────────
export const $yolo = atom<boolean>(false);

export function setYolo(sessionID: string, enabled: boolean) {
  if (!sessionID) {
    console.warn("[setYolo] No session ID provided");
    return;
  }
  console.log("[setYolo] Called with:", { sessionID, enabled, currentYolo: $yolo.get() });
  ws.send("set_yolo", { sessionID, enabled });
  console.log("[setYolo] Sent set_yolo to backend");
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

export function cancelQueuedSummarize(sessionID: string) {
  ws.send("cancel_queued_summarize", { sessionID });
}

export function setSummarizeQueued(sessionID: string, queued: boolean) {
  const s = new Set($summarizeQueued.get());
  if (queued) s.add(sessionID);
  else s.delete(sessionID);
  $summarizeQueued.set(s);
}

export function deleteMessagePart(messageID: string, partIndex: number) {
  ws.send("delete_message_part", { messageID, partIndex });
}

export function updateMessagePart(messageID: string, partIndex: number, content: string) {
  ws.send("update_message_part", { messageID, partIndex, content });
}

export function togglePinMessage(messageID: string, pinned: boolean) {
  ws.send("toggle_pin_message", { messageID, pinned });
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

  // Delete the user message itself and everything after it (agent responses),
  // then resend — this avoids a duplicate since send_message creates a new entry.
  const toDelete = msgs.slice(idx).map((m) => m.ID);
  if (toDelete.length > 0) {
    deleteMessages(toDelete);
  }

  logClientEvent("rerun_message", { sessionID, contentPreview: text.slice(0, 200) });
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

// Select (only add, never remove) all IDs in the given array
export function selectMessageIDs(ids: string[]) {
  const s = new Set($selectedMessageIDs.get());
  for (const id of ids) s.add(id);
  $selectedMessageIDs.set(s);
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

// ── Settings actions ───────────────────────────────────────────────────────────

export function addContextPath(path: string) {
  ws.send("add_context_path", { path });
}

export function removeContextPath(path: string) {
  ws.send("remove_context_path", { path });
}

export function addSkillsPath(path: string) {
  ws.send("add_skills_path", { path });
}

export function removeSkillsPath(path: string) {
  ws.send("remove_skills_path", { path });
}

export function initializeProject(msgID?: string) {
  ws.send("initialize_project", {}, msgID);
}

export function addCustomProvider(payload: {
  id: string; name?: string; type: string; baseUrl: string; apiKey?: string;
  models?: { id: string; name: string; contextWindow?: number; costPer1mIn?: number; costPer1mOut?: number }[];
}, msgID?: string) {
  ws.send("add_custom_provider", payload, msgID);
}

export function removeCustomProvider(id: string, msgID?: string) {
  ws.send("remove_custom_provider", { id }, msgID);
}

export function updateCustomProvider(payload: {
  oldId: string; id: string; name?: string; type: string; baseUrl: string; apiKey?: string;
  models?: { id: string; name: string; contextWindow?: number; costPer1mIn?: number; costPer1mOut?: number }[];
}, msgID?: string) {
  ws.send("update_custom_provider", payload, msgID);
}
