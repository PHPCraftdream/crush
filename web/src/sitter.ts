// sitter — "babysitter" loop that watches a session and kicks it back
// to life when the agent stalls. Use case: long-running tasks where the
// upstream provider (z.ai, Claude, ...) intermittently stops streaming,
// the watchdog has already given up, and the operator wants the run to
// auto-resume without manually pasting "continue" every 10 minutes.
//
// Rules per tick:
//   1. If the watched session has zero outstanding todos (no pending /
//      in_progress) → sitter shuts itself down. The work is done.
//   2. If the session is currently busy → still running, no nudge.
//   3. If the session is idle AND has pending work → send a short resume
//      prompt as if the operator had typed it. Goes through the normal
//      WS send_message path so it appears in history.
//
// Toggled via the chat input: `/sitter` (default 10 min), `/sitter 5`
// (5 min), `/sitter off`. State persists via localStorage so a page
// reload doesn't drop the sitter (we restore on app mount).

import { atom } from "nanostores";
import { $sessions, $busySessions, $activeSessionID } from "./store";
import { ws } from "./ws";

const LS_KEY = "crushSitter";
const DEFAULT_MINUTES = 10;

// Short resume prompt sent on each "stalled + has work" tick. Kept in
// Russian because that's the working language of this fork's operator;
// the agent will pick up the conversation language from the history.
const RESUME_PROMPT = "продолжай";

export interface SitterStatus {
  running: boolean;
  sessionID?: string;
  intervalMin?: number;
  startedAt?: number;
  lastTickAt?: number;
  lastNudgeAt?: number;
}

export const $sitter = atom<SitterStatus>({ running: false });

let timer: number | undefined;

function persist(status: SitterStatus) {
  if (status.running && status.sessionID && status.intervalMin) {
    localStorage.setItem(LS_KEY, JSON.stringify({
      sessionID: status.sessionID,
      intervalMin: status.intervalMin,
    }));
  } else {
    localStorage.removeItem(LS_KEY);
  }
}

function setStatus(next: SitterStatus) {
  $sitter.set(next);
  persist(next);
}

function tick() {
  const s = $sitter.get();
  if (!s.running || !s.sessionID) return;
  const sess = $sessions.get().find((x) => x.ID === s.sessionID);
  if (!sess) {
    // session vanished (deleted in another tab) — stop quietly.
    stopSitter();
    return;
  }
  // Read-only follow mode: another live crush process drives this session;
  // any send_message we fire would be ignored at best and confuse history
  // at worst. Skip — sitter stays armed for if/when ownership reverts.
  if (sess.OwnedExternal) {
    setStatus({ ...s, lastTickAt: Date.now() });
    return;
  }
  const todos = sess.Todos ?? [];
  const hasPending = todos.some((t) => t.status === "pending" || t.status === "in_progress");
  if (!hasPending) {
    // Job done — sitter retires itself.
    stopSitter();
    return;
  }
  const busy = $busySessions.get().has(s.sessionID);
  if (busy) {
    // Agent is still chewing on something. Don't nudge — give it time.
    setStatus({ ...s, lastTickAt: Date.now() });
    return;
  }
  // Idle + has work → send a resume prompt as a regular user message.
  ws.send("send_message", { sessionID: s.sessionID, content: RESUME_PROMPT });
  setStatus({ ...s, lastTickAt: Date.now(), lastNudgeAt: Date.now() });
}

export function startSitter(sessionID: string, intervalMin: number) {
  if (!sessionID) return;
  const mins = intervalMin > 0 ? intervalMin : DEFAULT_MINUTES;
  if (timer !== undefined) {
    clearInterval(timer);
    timer = undefined;
  }
  const now = Date.now();
  setStatus({
    running: true,
    sessionID,
    intervalMin: mins,
    startedAt: now,
    lastTickAt: now,
  });
  timer = window.setInterval(tick, mins * 60 * 1000);
}

export function stopSitter() {
  if (timer !== undefined) {
    clearInterval(timer);
    timer = undefined;
  }
  setStatus({ running: false });
}

// installSitterAutoRestore re-arms the sitter from localStorage on app
// mount. Call once near app bootstrap. If the saved session no longer
// exists on the server, the first tick will quietly drop the sitter.
export function installSitterAutoRestore() {
  try {
    const raw = localStorage.getItem(LS_KEY);
    if (!raw) return;
    const saved = JSON.parse(raw) as { sessionID?: string; intervalMin?: number };
    if (saved?.sessionID && saved.intervalMin && saved.intervalMin > 0) {
      startSitter(saved.sessionID, saved.intervalMin);
    }
  } catch {
    // Corrupted state — wipe and move on.
    localStorage.removeItem(LS_KEY);
  }
}

// handleSitterCommand parses a slash-command form typed in the chat
// input. Returns true if the input was a sitter command (caller should
// suppress sending to the server); false if it was something else.
//
// Forms accepted:
//   /sitter            → toggle. ON → off. OFF → start with default 10m.
//   /sitter N          → start (or restart) with N minutes (N is a positive integer).
//   /sitter off|stop   → explicit stop.
export function handleSitterCommand(text: string): boolean {
  const trimmed = text.trim();
  if (!/^\/sitter(\b|$)/i.test(trimmed)) return false;
  const parts = trimmed.split(/\s+/);
  const arg = parts[1]?.toLowerCase();
  const targetSession = $sitter.get().sessionID || $activeSessionID.get();
  if (!targetSession) return true; // nothing to attach to, swallow silently
  if (arg === "off" || arg === "stop" || arg === "0") {
    stopSitter();
    return true;
  }
  if (!arg) {
    // toggle
    if ($sitter.get().running) stopSitter();
    else startSitter(targetSession, DEFAULT_MINUTES);
    return true;
  }
  const n = parseInt(arg, 10);
  if (!Number.isFinite(n) || n <= 0) {
    // unrecognized argument; treat as start with default
    startSitter(targetSession, DEFAULT_MINUTES);
    return true;
  }
  startSitter(targetSession, n);
  return true;
}
