# Plan: Unify the one-logical-call state machine (P2-4), staged

Status: **NOT STARTED — deferred by explicit user decision on 2026-08-12.** This document captures
what was task #415 (P2-4 implementation) as a plan for a future round, then removed from the active
TaskList. Do not begin implementation without a fresh, explicit go-ahead from the user.

## Origin

Source: `docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md`, finding P2-4. Full
investigation and staged design: `docs/design/2026-08-12-state-machine-unification.md` (read-only
research via `@ox`, point-in-time `3a145e60`, all references are to file+function since the code
was actively changing in parallel worktrees at investigation time).

This plan supersedes `docs/plans/2026-08-10-session-executor-consolidation.md`'s "re-scope first"
step for this specific subsystem — the design doc above **is** that re-scope, produced after
tasks #337-348 (and this round's #404-#413) landed. The 2026-08-10 plan's remaining
recommendations (table-driven state-machine tests, multi-session delegation discipline, freeze
after consolidation) still apply and should be read alongside this document.

## Why this is a plan, not a task

The design doc's own recommendation (§8) is explicit: **do not gate the release on this
refactor.** The three release blockers and P1 findings of the 2026-08-12 round are closed by
point fixes (`:execrows` for P0-2, ticker-lifecycle join for P1-4, dropping `mb.replacement`
after durable enqueue for P0-1, a durable marker before "accepted" for P0-3) — days, not weeks.
Doing the full refactor first would mean carrying bugs into a new structure while simultaneously
changing behavior and shape, which is not reviewable and makes any regression indistinguishable
from "refactor fallout." That work is tracked separately in this round's own task list, not here.

## What the investigation found (summary — see design doc for full detail)

- **Five independent "accept doors"** (direct turn, web interrupt, cross-process interrupt inject,
  non-interrupt inject, durable queue, orphan handoff) each give a different durability guarantee
  on "accepted." `handleInterruptTick`'s double-ownership (this round's P0-1) is the direct
  consequence of two of these doors writing to the same call without a shared fence.
- **A new defect found during the investigation, not in the original review report**: in
  `agent.runTurn`'s `shouldSummarize` branch, `mb.submit(call, nil)` is used to requeue a
  post-compaction continuation. For a `FromDurableQueue` call, `mailbox.submit`'s P0-1 guard
  silently drops it, `runTurn` returns `nil`, and the pump `Ack`s (deletes) the row — the
  continuation is lost with no trace. This is the same *class* of bug P2-4 describes (a local
  guard added for one call site missed that `submit` is also used as a plain "queue this" call).
  **This should be verified and probably filed as its own task before this plan is picked up**,
  independent of whether the full refactor proceeds — see "Immediate follow-up" below.
- The target model (`accepted -> durable(rowID) -> leased(owner,generation) -> running ->
  committed | retryable | terminal_failed`) structurally closes **P0-1 and P0-3 only** (2 of 3
  blockers), and **P1-4** (1 of 4 P1 findings). It does **not** close P0-2, P1-2, P1-3, or P2-1 —
  those are independent API/config defects, not state-machine fragmentation, and need their own
  point fixes regardless of whether this refactor happens.
- `session_run_queue` (pending/leased/attempts/terminal_failure/idempotency key) is already
  roughly 70% of the durable half of the target model. This is not a green-field rewrite.

## Staged plan, if/when resumed (full detail in the design doc §5)

Ordered by value/risk, each step an independently mergeable PR with its own gate:

0. **Map hygiene** (near-zero risk, ~half a day): delete the dead interrupt-recovery paths
   (`requeueInterruptMessage`, `recreatePendingInjectRowPostAccept`, non-atomic
   `ConsumeInterruptInject`), reconcile the undocumented/unimplemented `InjectID` cleanup contract,
   fix the P2-3 stale comments. Makes the later steps legible — right now it's not possible to
   tell "this path is live" from "this path is only kept alive by a test."
1. **Durable-first accept** (the step that carries most of the value): one admission primitive in
   `session.Service` — `AcceptCall(ctx, sessionID, logicalCallID, callData) (ticket, error)` —
   routed through all five accept doors. Removes `startBoundedDetachedRun` entirely. Structurally
   closes P0-3, incidentally closes SEC-1 (the log-leaking function disappears with it), and makes
   UI busy-state derivable from durable state instead of `mailbox.state`. Estimated ~60-70% of the
   full refactor's value for ~25% of its scope.
2. **Single owner token** (rowID + fence column): `mailbox` holds a ticket instead of a bare
   `SessionAgentCall`; era and lease become two projections of one token.
   `interruptAndReplace` refuses a call whose lease this process doesn't hold. Structurally closes
   P0-1 — a second owner becomes inexpressible in the types, not just guarded against.
3. **Mailbox loses its durability policy**: remove the `FromDurableQueue` branch from
   `mailbox.submit`; mailbox becomes purely ordering/wakeup. Closes the newly-found compaction
   continuation loss (see above) and the other three insertion paths that don't currently agree
   with `submit`'s guard.
4. **Fenced/observable message writes** (independent of steps 1-3, can move at any time): change
   `UpdateMessageIfNotTerminal` to `:execrows`, have `message.Service.Update` return
   `(applied bool, err error)` and only publish when `applied`. This is the actual P0-2 fix and
   should not wait on the rest of this plan.
5. **Unified cancellation authority**: collapse `activeRequests` / `mb.current.cancel` /
   `mb.dispatcherCancel` / the pump's `execCancel` into one per-era owner object. Interrupt-watcher
   becomes session-level and joinable (closes P1-4 structurally); the P1-1 lease watchdog becomes a
   timer on the same object instead of three uncoordinated ones.
6. **File-splitting — last, and only if 1-5 land.** Not a goal on its own: the problem is that
   "owner" is five different words in five subsystems, not that `agent.go` is 5046 lines. Splitting
   files before unifying ownership just spreads the same confusion across more files.

## Recommendation on resume (from the design doc §8)

1. Ship the release with point fixes only (this round's #404-#413), not this refactor.
2. If picked up, fund **step 1 alone** first as a scoped increment — it has the best value/risk
   ratio and does not require the schema/fence work of step 2.
3. Steps 2-3 (owner token + mailbox cleanup) — only if a P0-1-class bug recurs after step 1. They
   are the most expensive and highest-risk steps because they touch
   `mailbox.drainOrReleaseFinal`, the most expensive-to-get-right invariant in the file.
4. Step 6 (file splitting) is never a goal on its own.
5. Expect real test cost: 27 test files in `internal/agent`/`internal/session` know mailbox
   internals directly; at least two existing tests currently assert the *broken* pre-fix behavior
   and will need rewriting under any of the above, not just the full refactor.

## Immediate follow-up (does not require this plan to start)

The compaction-continuation loss found during this investigation (§O-2 in the design doc) looks
like a real, independent bug reachable today. Verify it against the current `main` (post round
#404-#413 merge) and, if confirmed, file it as its own task — it does not depend on any part of
this refactor plan and should not wait on a decision about the plan.

## Explicit non-goals for this plan document

- This document does **not** authorize starting the work.
- This document does **not** commit to which step (if any) happens next, or when — that is a user
  decision.
- This document does **not** claim the design doc's map is exhaustive going forward — code in this
  area keeps moving; re-verify file+function references before resuming, not line numbers (the
  design doc deliberately avoids line numbers for this reason).
