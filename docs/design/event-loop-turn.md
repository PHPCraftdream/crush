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

## 3. Phase 4 — wake-on-event autonomous idle-resume (IMPLEMENTABLE SPEC)

Status of this section: **approved-to-implement** (the user delegated the
design-gate approval, "решай всё сам в сторону красоты"). Phases 1–3 are
shipped + CI-green. This section is the implementable spec the impl tasks
(#82–#85) build against.

**Goal:** when a background job finishes and the owning session is **idle**, the
agent continues on its own — no human nudge, no poll loop — exactly once per
batch of completions, bounded so it can never run away. This is the realization
of "свой тик-таймер + ловить сообщения" for the common case (one or more
backgrounded builds/tests finishing after the turn that launched them ended).

### 3.1 Shape A vs Shape B — and why we build A

- **Shape B — true mid-turn suspend/resume.** A turn that blocks on "I need X"
  serialises a *suspension* (`{session, waiting_on: job}`), unwinds the
  goroutine, and is rehydrated on the event. This changes the **turn
  lifecycle**: "active" stops meaning "a goroutine is parked in `Run`", which
  forces an explicit suspended state in the session row, restart recovery
  (mirroring `recoverInterruptedTurns`), watchdog reconciliation (a suspended
  turn has no live stream), and summarization/cost reconciliation across the
  gap. High blast radius on the core turn loop. **Deferred** — not needed for
  the goal.
- **Shape A — wake-on-event supervisor (THIS DESIGN).** We never suspend a live
  turn. A turn ends normally (the model backgrounds a job and finishes its
  step). When the job later completes, Phase 3 already delivers a message into
  the session; Phase 4 adds *one decision*: if the session is idle and autonomy
  is enabled, **start a fresh turn** over that just-delivered message instead of
  only persisting it. Idle→busy and the dedup of concurrent completions are
  already solved by the existing `Run`/`IsSessionBusy`/`messageQueue`
  machinery — so Shape A is almost entirely *reuse*, with no turn-loop surgery.

### 3.2 The one mechanism (and why it's tiny)

The crux discovery (from reading `agent.go` / `coordinator.go`):

- `sessionAgent.Run(call)` (agent.go:359) **already single-flights per session**:
  if `IsSessionBusy(sessionID)` it appends the call to `messageQueue` and
  returns `nil,nil` (agent.go:368–376); otherwise it claims the slot
  (`activeRequests.Set`, agent.go:498) and runs, persisting the prompt itself
  via `createUserMessage` (agent.go:487).
- `IsSessionBusy` = "is there a live `cancelFunc` in `activeRequests` for this
  id" (agent.go:2204).
- Phase 3's `notifyBackgroundJobDone` (coordinator.go:1607) currently builds the
  summary and calls `InjectMessage`, which **only persists** when the session is
  idle (agent.go:2162–2172 — it latches into `injectQueue` only when busy).

Therefore Phase 4 is a single branch inside `notifyBackgroundJobDone`:

```
build summary (unchanged)
if autoResumeEligible(sessionID):
    bumpConsecutiveResume(sessionID)        // guardrail counter
    go c.Run(detachedCtx, sessionID, summary)   // persists + starts OR queues
else:
    c.InjectMessage(detachedCtx, sessionID, summary)   // Phase 3 behavior, unchanged
```

- **Idle session** → `Run` claims the slot and starts a turn over the summary.
- **Two jobs finish near-simultaneously** → the first `Run` claims the slot; the
  second `Run` sees `IsSessionBusy` and lands in `messageQueue`, folded into the
  same turn's continuation. **Single-flight and coalescing come for free** — no
  new lock, no `sync.Once`, no supervisor goroutine.
- **Busy session (autonomy on)** → `Run` queues too; equivalent to the Phase-3
  merge, so behavior is unchanged for the busy case.

`autoResumeEligible(sessionID)` is the whole policy surface:

```
return c.autonomyEnabled()            // Options.AutoResumeOnJobDone == true
   && c.persistentMode                // NOT crush run (single-turn, process exits)
   && consecutiveResume(sessionID) < maxConsecutiveAutoResumes
   && withinRunLimits()               // MaxCost / MaxTokens not exhausted
```

Note we do **not** pre-check `IsSessionBusy` here: `Run` already does the right
thing for busy/idle. Eligibility is purely the autonomy policy.

### 3.3 Guardrails (the safety surface)

1. **Opt-in kill-switch.** `config.Options.AutoResumeOnJobDone *bool`, **default
   OFF** (nil ⇒ off — note this is the opposite default from Phase 3's
   `NotifyOnBackgroundJobDone`, which defaults on; autonomy must be deliberately
   enabled). When off, Phase 4 is fully inert and behavior == Phase 3.
2. **Termination bound.** A per-session counter
   `consecutiveAutoResumes[sessionID]`, incremented on each auto-resume,
   **reset to 0 by any genuinely human message** into the session. Cap =
   `maxConsecutiveAutoResumes` (proposed const **5**; revisit if a real workload
   needs more). At the cap, fall back to `InjectMessage` (still delivered +
   web-visible) and stop auto-resuming until a human speaks. This is the
   anti-runaway invariant: an agent that keeps backgrounding jobs that keep
   finishing cannot loop forever without human involvement.
   - "Human message" reset point: the operator send path — the web
     `handleSendMessage` → `coordinator.Run`/`InterruptAndSend`. Reset the
     counter there, NOT in `InjectMessage` (which auto-resume itself uses).
3. **Never for `crush run`.** `RunNonInteractive` is single-turn and the process
   exits at end of turn; an OnDone firing after that has nowhere to go. Gate on a
   coordinator `persistentMode bool` (true only for the long-lived web/interactive
   coordinator; false for `crush run`). Belt-and-suspenders: even if it fired, the
   process is gone.
4. **Respect cost/token caps + cancel.** Reuse the existing `maxCost`/`maxTokens`
   (coordinator.go:279 `SetRunLimits`) — if the session has hit its budget, do
   not auto-resume. A `Cancel(sessionID)` mid-auto-turn behaves exactly like any
   cancelled turn (the auto-started `Run` owns the `activeRequests` slot).
5. **Detached, non-blocking.** The call runs from the OnDone goroutine, already
   detached (`context.Background()` + timeout in Phase 3). Use `go c.Run(...)`
   so the OnDone goroutine never blocks for the whole turn; the auto-turn's own
   lifecycle (watchdog, cancel, persistence) governs it.

### 3.4 Web UX marker (operator must always see autonomy)

Autonomy is invisible-by-default danger; the operator must always be able to
tell "the agent continued on its own". Mechanism:

- Mark the auto-started turn. Cleanest: tag the injected/persisted *user
  message* that triggers the resume with a metadata flag (e.g.
  `message.Metadata`/an `AutoResumed bool` on the summary message, or a distinct
  prefix the web can detect). The summary text already reads "Background job …
  finished"; the flag lets the web render a badge rather than parse prose.
- Backend: include the flag in the WebSocket message/event payload the web
  already receives for new messages (same pubsub path as Phase 3 delivery).
- Web (`web/`): render a small marker on that message —
  **"↻ auto-resumed: background job finished"** — so the timeline visibly shows
  where the agent woke itself. No localStorage, no client-side persistence
  (fork rule).

### 3.5 Lifecycle / recovery / non-goals

- **No cross-restart job recovery.** Background shells die with the process
  (existing behavior). On restart we do NOT resurrect pending jobs nor
  auto-resume anything — startup never auto-starts a turn. Recovery of
  *interrupted turns* stays exactly as `recoverInterruptedTurns` does today;
  Phase 4 adds nothing to the startup path.
- **Cancellation / session close.** On session close the background shell is
  dropped/killed as today; a completion arriving for a closed session falls to
  the Phase-3 `InjectMessage` debug-log path (delivery fails quietly). No new
  registration to clean up — there is no supervisor goroutine to leak.
- **Watchdog / summarization / cost.** An auto-resumed turn is an ordinary turn:
  it gets the stream watchdog, summarization, and cost accounting for free. No
  special-casing.
- **Explicitly NOT in scope:** Shape B suspend/resume, a per-session supervisor
  goroutine, cross-session wake fan-out beyond the existing foreign-owner-follow,
  and any persisted "waiting_on" state.

### 3.6 Exact integration points (for impl #82–#84)

- **#82 — config + primitives.**
  - `internal/config/config.go`: add `Options.AutoResumeOnJobDone *bool`
    (default OFF; document the opt-in + the contrast with
    `NotifyOnBackgroundJobDone`).
  - `internal/agent/coordinator.go`: add the consecutive-resume counter
    (a `map[string]int` guarded by a mutex, or reuse an existing csync map type
    used elsewhere in the file), `persistentMode bool` field + constructor wiring
    (true for the web coordinator, false for the `crush run` path), and a
    `maxConsecutiveAutoResumes` const (5). Helpers: `autonomyEnabled()`,
    `consecutiveResume(id)`, `bumpConsecutiveResume(id)`,
    `resetConsecutiveResume(id)`. Pure-unit-testable; no behavior wired yet.
- **#83 — wake-supervisor (the branch).**
  - `internal/agent/coordinator.go`: implement `autoResumeEligible(sessionID)`
    and the branch in `notifyBackgroundJobDone` (go c.Run vs InjectMessage).
  - Wire `resetConsecutiveResume` into the human send path (web
    `handleSendMessage` → `coordinator.Run`/`InterruptAndSend`; locate exact
    call site in the websocket/app layer).
  - Tests: idle→auto-resume fires; busy→merge unchanged; two completions →
    single turn + one queued (single-flight); counter hits cap → falls back to
    InjectMessage; kill-switch OFF → pure Phase-3 behavior; human message resets
    the counter.
- **#84 — web marker.**
  - Backend: flag on the resume message + event payload.
  - `web/`: render the "↻ auto-resumed" badge. Playwright/e2e if cheap; else a
    unit/render test of the badge condition.

### 3.7 Open questions resolved toward beauty (flag for the gate)

- **Default OFF** for `AutoResumeOnJobDone` — autonomy is opt-in. (Resolved.)
- **maxConsecutiveAutoResumes = 5** — generous enough for a normal
  build→test→fix chain, low enough to bound runaway. (Resolved; trivially
  tunable.)
- **No new config for the cap** initially — a const keeps the surface small; we
  promote it to `Options` only if a real workload needs per-project tuning.
  (Resolved: const first.)
- **Reset on human message, not on time** — time-based reset would let a slow
  human-then-silence pattern drift; message-based reset is crisp and matches the
  "human is back in the loop" intent. (Resolved.)

## 4. Recommendation

1. **Phases 1–2** — shipped + CI-green (the never-freeze fix). Done.
2. **Phase 3** — shipped (push job-completion via `InjectMessage`). Done.
3. **Phase 4 (this spec, §3)** — approved to implement as Shape A: a single
   eligibility-gated branch in `notifyBackgroundJobDone` that calls `Run`
   (reusing its built-in single-flight) instead of only `InjectMessage`, plus
   the opt-in kill-switch, the consecutive-resume bound, and the web marker.
   Shape B (true mid-turn suspend) remains deferred behind a real need.
