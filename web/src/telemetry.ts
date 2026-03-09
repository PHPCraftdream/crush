/**
 * telemetry.ts
 *
 * - logClientEvent(): sends user events to the backend when debug mode is on.
 * - JS errors / unhandled rejections are always forwarded to the backend.
 *
 * Errors that occur before WS is open are buffered and flushed on connect.
 */

import { $config } from "./store";
import { ws } from "./ws";

// ── Error buffer (pre-connect) ─────────────────────────────────────────────

interface ErrorPayload {
  message: string;
  source?: string;
  stack?: string;
}

const errorBuffer: ErrorPayload[] = [];
let wsReady = false;

function flushErrors() {
  while (errorBuffer.length) {
    ws.send("log_client_error", errorBuffer.shift());
  }
}

function sendError(payload: ErrorPayload) {
  if (wsReady) {
    ws.send("log_client_error", payload);
  } else {
    errorBuffer.push(payload);
  }
}

// ── Public API ─────────────────────────────────────────────────────────────

export function logClientEvent(event: string, details?: Record<string, unknown>) {
  const cfg = $config.get();
  if (!cfg?.debug) return;
  ws.send("log_client_event", { event, details });
}

// ── Setup (call once at startup) ───────────────────────────────────────────

export function setupTelemetry() {
  // Flush buffered errors once WS connects.
  ws.on("_connected", () => {
    wsReady = true;
    flushErrors();
  });
  ws.on("_disconnected", () => {
    wsReady = false;
  });

  // Forward all uncaught JS errors.
  window.addEventListener("error", (ev) => {
    sendError({
      message: ev.message ?? String(ev.error),
      source: ev.filename ? `${ev.filename}:${ev.lineno}:${ev.colno}` : undefined,
      stack: ev.error?.stack,
    });
  });

  // Forward unhandled promise rejections.
  window.addEventListener("unhandledrejection", (ev) => {
    const reason = ev.reason;
    sendError({
      message: reason?.message ?? String(reason),
      stack: reason?.stack,
    });
  });
}
