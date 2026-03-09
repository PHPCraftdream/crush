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

// Per-session model overrides: sessionID → model key ("large" | "small")
export const $sessionLargeModel = atom<Record<string, string>>({});
export const $sessionSmallModel = atom<Record<string, string>>({});

// Last agent error to display in chat
export const $agentError = atom<string | null>(null);

export function setSessionLargeModel(sessionID: string, model: string) {
  $sessionLargeModel.set({ ...$sessionLargeModel.get(), [sessionID]: model });
}
export function setSessionSmallModel(sessionID: string, model: string) {
  $sessionSmallModel.set({ ...$sessionSmallModel.get(), [sessionID]: model });
}
