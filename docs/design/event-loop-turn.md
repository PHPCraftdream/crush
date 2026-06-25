# Design — push job-completion & event-loop turn (Phases 3–4)

Status: **proposal, not implemented**. Phases 1–2 (the never-freeze backstop +
responsive `job_output` polling) are shipped and verified. This doc designs the
remaining, larger pieces so they can be approved before any core-turn code is
touched.

## 0. Where we are after Phases 1–2

- **Phase 1a** — the stream watchdog pauses its idle timer while a tool runs,
  but the pause is now BOUNDED by `toolMaxDuration` (default 15m,
  `Options.StreamToolTimeoutSeconds`). A stuck tool can no longer freeze the
  turn — it surfaces as a `Tool timeout` finish.
- **Phase 1b** — `job_output --wait` is bounded (`jobOutputMaxWait`, 90s):
  it returns `Status: running` instead of blocking forever.
- **Phase 2** — a `wait:true` poll returns early on completion OR new output
  (`BackgroundShell.WaitForChange`), and every result carries `elapsed` + exit
  code. Polling is cheap and responsive.

The orchestrator can no longer hang. What remains is **eliminating the poll
loop itself** so the model doesn't spend turns asking "is it done yet?".

## 1. The run-vs-interactive split (the key constraint)

There are two execution models, and the push/event-loop work applies to only
one of them:

- **`crush run` (non-interactive)** — `app.RunNonInteractive` runs ONE turn and
  the process exits. A background job that finishes *after* the turn ends has
  nowhere to be delivered — the process is gone. Here the only correct model is
  the Phase 1–2 bounded poll: the model must finish its work within the run,
  polling as needed. **Phases 3–4 do NOT apply to `crush run`.**
- **Interactive / web (persistent coordinator)** — the coordinator stays alive
  driving N long-lived sessions over WebSocket. A session can receive new input
  (`InjectMessage`) at any time. THIS is where pushing a completion and an
  event-loop make sense.

Everything below is scoped to the persistent/interactive model.

## 2. Phase 3 — push background-job completion into a live session

**Goal:** when a backgrounded command finishes, its result is delivered to the
owning session as a fresh message, so the model reacts when it's ready instead
of polling.

**Reuse what exists:**
- `shell.GetBackgroundShellManager()` / `BackgroundShell` (has a `done` chan,
  `GetOutput()`, `Elapsed()`, exit via `shell.ExitCode`).
- `Coordinator.InjectMessage(ctx, sessionID, prompt, …)` — persists a user/
  system message and schedules it into the next provider request WITHOUT
  cancelling the in-flight turn (the fork's live-inject). This is exactly the
  delivery channel we want.
- `tools.GetSessionFromContext(ctx)` — the bash tool already knows the owning
  session id when it backgrounds a command.

**Mechanism:**
1. `BackgroundShell` gains a completion signal — it already closes `done`; add a
   registration API on the manager: `OnComplete(shellID, func(BackgroundShell))`
   (fired once, from a single watcher goroutine per shell that selects on
   `done`).
2. When `bash` auto-backgrounds a command, it captures `sessionID :=
   GetSessionFromContext(ctx)` and registers interest:
   `bgManager.OnComplete(shell.ID, notify(sessionID, shell.ID))`.
3. On completion the callback builds a concise system message —
   `"Background job <id> (<command>) finished: exit <N>, <elapsed>.\n<truncated output>"`
   — and calls `coordinator.InjectMessage(detachedCtx, sessionID, msg)`.

**Invariants (the hard parts):**
- **Never cancel a live turn.** Use `InjectMessage` (merge into next request) /
  the message queue, never `Cancel`. If the session is mid-turn, the completion
  rides along on the next provider call; if idle, it kicks a fresh turn (same as
  any injected user message).
- **Idempotent** — exactly one inject per job. The watcher fires once (`done` is
  closed once); guard with a `sync.Once` per shell so a double-registration or a
  replayed completion can't double-inject.
- **Ordering** — inject in completion order; rely on `InjectMessage`'s existing
  persistence ordering. Don't try to interleave with partial output already
  shown.
- **Detached context** — the callback fires from the shell's watcher goroutine,
  not the turn; use `context.WithoutCancel` + a short timeout for the DB write so
  a cancelled turn doesn't drop the completion.
- **Opt-in / noise control** — only register interest when the model
  backgrounded a command in a session that's still open. On session close,
  drop the registration (and optionally kill the shell, as today). Consider a
  per-session cap so a fan-out of 50 background jobs doesn't bury the model.

**Why this is safe-ish:** it's additive — it rides the existing inject path and
the background-shell lifecycle. It does NOT touch the turn loop. Risk is mostly
in noise/ordering, not in correctness of the core agent.

**Open question for approval:** should completion auto-inject *always*, or only
when the model opted in (e.g. a `bash(..., notify_on_done=true)` arg)? Default
proposal: auto-inject, with a per-session cap and a config kill-switch.

## 3. Phase 4 — suspend/resume turn via a per-session event loop

**Goal:** the deepest form of "don't wait" — a turn that needs an external
result *ends cleanly* and is *resumed by an event*, with zero polling and zero
blocking. This is the full realization of "свой тик-таймер + ловить сообщения".

**Shape:**
- A turn that hits an "I need X which isn't ready" point returns a structured
  *suspension* instead of blocking: persist `{session, waiting_on: job <id>}`.
- A per-session supervisor (one goroutine, or a shared tick multiplexer) selects
  on the session's event sources:
  - background-job completion (Phase 3's signal),
  - injected user messages,
  - cross-session signals (the existing foreign-owner-follow).
- On any event it RESUMES the session: re-enters the agent with the event folded
  in (via the same queue/inject path).

**Why it's the big one (risk):**
- It changes the **turn lifecycle**: "active" can no longer mean "a goroutine is
  blocked in Run". Need an explicit suspended state in the session row + recovery
  on restart (mirrors the existing `recoverInterruptedTurns`).
- **Wakeup dedup** — multiple events must not spawn concurrent turns for one
  session; needs the same single-flight discipline as `IsSessionBusy` + the
  message queue.
- **Cancellation** — cancelling a suspended session must clean up its
  registrations and watchers.
- Interplay with summarization, the watchdog (a suspended turn has no live
  stream to watch), and cost accounting.

**Incremental path (recommended):** Phase 3 alone already removes most polling
for the common case (one backgrounded build/test). Phase 4 is only worth it if
sessions routinely juggle *several* concurrent long jobs or need true
fire-and-forget. Build Phase 3 first; measure; do Phase 4 only if the poll loop
is still a real cost.

## 4. Recommendation

1. **Ship Phases 1–2 now** (done + full pre-push green) — the never-freeze fix
   the user asked for.
2. **Phase 3 next** if zero-poll-in-web is wanted — additive, reuses
   `InjectMessage`, moderate risk, big UX win for the web UI.
3. **Phase 4** — defer behind Phase 3 + a real need; it's a core-turn redesign
   and should not be entered without the suspended-turn lifecycle fully spec'd
   and a recovery story.

No core-turn code should be written for Phases 3–4 until this is approved.
