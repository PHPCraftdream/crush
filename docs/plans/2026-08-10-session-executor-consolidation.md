# Plan: Consolidate the session state machine into a single SessionExecutor, then a stabilization freeze

Status: **NOT STARTED — deferred by explicit user decision on 2026-08-10.** This document
captures task #349 (P3) as a plan for a future round. Do not begin implementation without a fresh,
explicit go-ahead from the user — the original review deliberately reserved this decision for the
user, not the agent, and the user's 2026-08-10 decision was to write this plan and defer execution.

## Origin

Source: `docs/reviews/2026-08-09-release-concurrency-followup-review.md`, findings P2-3 and P2-4.
The review explicitly labeled this as work to do **after** the release blockers close, not before —
tasks #337-348 (all P0/P1/P2 blockers from both 2026-08-09 review rounds) are now closed and
independently verified; see `docs/release_gate_summary.md` and `docs/release_gate_report.md` for
the full acceptance evidence. This plan is the only thing standing between "blockers closed" and
"start the consolidation" — it exists so the review's recommendation isn't lost, not as an
authorization to proceed.

## P2-3 — Fragmented session state machine

Critical concurrency logic is spread across:
- `internal/agent/agent.go` (~4,000+ lines)
- `internal/agent/mailbox.go` (~1,000+ lines)
- `internal/agent/coordinator.go` (~2,700+ lines)
- WebSocket handlers (`internal/server`/web-facing dispatch)
- `internal/session/lock.go` (OS-level session lock)

This fragmentation is what let tasks #337-347 fix one path while leaving a sibling with a
different guarantee — concretely, **three separate detached-run policies** existed
side by side before task #340 unified them:
- `restartOrphaned`
- `restartOrphanedWithRetry`
- `startDetachedRun`

(Task #340 replaced all three call sites with one durable `session_run_queue` + `RunQueuePump`
path — see `internal/session/run_queue_pump.go` and `internal/agent/agent.go`'s
`restartOrphanedWithRetry`/`startDetachedRun`, both of which now funnel through
`session.EnqueueRunQueueEntry`. This closed a meaningful share of the P2-3 fragmentation already;
see "What #340 already covers" below.)

**Review's recommendation:** extract a single `SessionExecutor` with explicit states and one
`Accept` method; move persistence/delivery policy out of the coordinator's and handlers'
goroutines. Verify invariants with table-driven state-machine tests instead of relying on which of
the (formerly three, now one) retry helpers produced a given exit path.

### What task #340 already covers

Task #340 (closed, `internal/session/run_queue_pump.go` + `internal/agent/agent.go`) unified the
three detached-run policies into one durable-queue path:
- `session_run_queue` DB table (see `internal/db/migrations/20260809000001_add_session_run_queue.sql`)
- `EnqueueRunQueueEntry` / `LeaseRunQueueEntry` / `AckRunQueueEntry` /
  `TerminalFailRunQueueEntry` / `NackRunQueueEntry`
- An independent `RunQueuePump` goroutine (production tick 3s, test-overridable via `TestTick`)
- `ErrCallAlreadyAttempted`/`AlreadyAttempted() bool` marker-interface classification (task #339)
  to prevent duplicate execution on retry

This means the *detached/orphaned-call* corner of P2-3 already has one canonical path. What
remains fragmented is everything else the review called out: the **owner-turn** state machine
itself (mailbox ownership transitions — `mbOwned`/`mbReleasing`/`mbIdle` in `mailbox.go`), the
`Run()`/`runTurn()` turn loop in `agent.go`, `coordinator.go`'s dispatch/model-override/summarize
orchestration, and the WebSocket handlers' own idea of session busy/queued state
(`internal/server`, task #342's busy/queued event publishing). **A future SessionExecutor's scope
should be re-assessed against the post-#340 codebase before starting** — some of what P2-3
describes may already be substantially narrower than when the review was written.

## P2-4 — Change velocity vs. stability

299 commits in 14 days at review time; a single follow-up round added 4,000+ lines, mostly tests
and long concurrency comments. Not a defect by itself, but for a stable release it means another
point patch on top of the current state machine has a high chance of closing one interleaving
while opening another — which the round that produced task #339 demonstrated directly (a
regression introduced by a commit in that same round).

**Review's recommendation (post-P0-fix):** a short stabilization freeze — no new features, only
the one canonical executor/queue path, race-oriented tests, and soak/fault-injection testing on
lock release, provider cancellation, cross-process contention, and shutdown.

## Recommended shape of a future round (not yet started)

1. **Re-scope first.** Before writing any code, re-read `agent.go`/`mailbox.go`/`coordinator.go`
   as they exist post-#337-348 (not as they existed when the review was written) and produce an
   updated map of exactly which states/transitions remain duplicated across files. Task #340
   already removed one whole axis of duplication (detached-run policy); the remaining scope may be
   smaller than the original review implies.
2. **Design the SessionExecutor's state enum and `Accept` contract** before touching call sites —
   this is an architecture decision, not a mechanical refactor, and deserves its own design doc
   (pattern: `docs/design/` in this repo, e.g. `docs/design/2026-08-04-session-owner-mailbox-design.md`
   as a precedent for this exact subsystem).
3. **Migrate call sites incrementally**, package by package, with the existing regression suite
   (`internal/agent/p0_*`, `p1_*`, `p3*`, `release_gate_test.go`, etc.) required to stay green at
   every step — this codebase's own session history shows concurrency-sensitive regressions are
   caught almost exclusively by hands-on revert-checks, not by trusting a refactor "looks
   equivalent."
4. **Add table-driven state-machine tests** for the new executor's transition table as the
   primary correctness tool, per the review's explicit recommendation — replacing the current
   pattern of many narrow, scenario-specific regression tests (`p0_2_*`, `p0_338_*`, `p339_*`,
   `p341_*`, etc.) with one canonical table, while keeping the scenario-specific tests as
   regression anchors during the migration.
5. **Only after the consolidation lands**, declare the stabilization freeze: no new features,
   only the one canonical path, plus soak/fault-injection tests specifically on lock release,
   provider cancellation, cross-process contention, and shutdown (the four areas task #337-348
   already hardened individually — a freeze period would validate they hold up under sustained,
   combined load rather than the current one-scenario-at-a-time test style).
6. **This is a multi-session effort.** Given the size of `agent.go`/`coordinator.go`, do not
   attempt this as a single delegated `/crush` round — decompose into a proper task list (per this
   repo's `/babygoal`/`/task` conventions) with its own investigation phase, before delegating.

## Explicit non-goals for this plan document

- This document does **not** authorize starting the work.
- This document does **not** commit to the freeze's duration, scope, or start date — those are
  user decisions, per the original review's own framing ("Это решение принимает пользователь, не
  агент").
- This document does **not** claim the post-#340 fragmentation assessment above is complete — it's
  a starting point for the re-scoping step, not a finished design.
