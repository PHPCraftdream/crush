# Release gate — 2026-08-12/13 follow-up round

Verdict point: `6b9abad6` (main). Source: `docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md`
("Минимальный release gate", 9 properties). This document is the composite
verification the review asked for explicitly — "После фиксов провести
stabilization freeze и один составной release gate, а не набор helper
tests" — not another round of isolated unit tests. It traces each property
to what actually closes it, states the confidence level honestly (some
properties rest on genuine empirical revert-checks, some rest on code
review because an empirical test proved unreliable to construct), and ends
with a single go/no-go call.

## Baseline: full suite, first attempt, clean

```
go build ./...                      — clean
golangci-lint run (v2.10)           — 0 issues
go test -short -failfast ./...      — every package ok, first attempt, no retry needed
```

Full package list passed: agent, agent/agentguard, agent/cliprovider,
agent/prompt, agent/tools, agent/tools/mcp, app, cmd, commands, config,
csync, db, deploy, diffdetect, discover, env, event, filetracker, fsext,
home, hooks, log, message, oauth, oauth/copilot, permission, platform,
projects, proto, pubsub, queue, server, session, shell, skills, stringext,
ui/notification, update, version.

This is the same gate CI runs (`.github/workflows/build.yml` +
`lint.yml`), run locally via `.githooks/pre-push` — first attempt, no
retry-once needed (the hook has a documented flaky-Windows-test retry
backstop; it wasn't triggered).

## Property-by-property

### 1. One cross-process interrupt → one durable execution identity, ≤1 provider/tool execution (incl. busy-owner case)

**Status: CLOSED, strong coverage.** `handleInterruptTick` marks the call
`FromDurableQueue=true`; `mailbox.interruptAndReplace` skips
`mb.replacement` for such calls (P0-1, commit `1ee26749`). Direct unit
tests exercise both branches of the guard
(`TestMailbox_InterruptAndReplace_DurableCallSkipsReplacement`,
`...NonDurableCallStillSetsReplacement`) plus the transactional
enqueue+interrupt path (`TestHandleInterruptTick`,
`TestP0_2_FaultInjection_InterruptTickAtomicTransaction`). Full
`internal/agent` package green under `-race` in isolation, twice.

### 2. Two consecutive interrupts → two consecutive active generations interrupted, correct FIFO order

**Status: CLOSED, adequate coverage with one disclosed gap.**
`startInterruptTicker` no longer exits after the first interrupt (P1-4,
commit `3ee3501d`); `PeekInterruptInject`/`DrainPendingInjects` gained a
`rowid ASC` tie-breaker after `created_at ASC` (P2-1, commit `079c2cef`)
so same-second injects resolve in insertion order.
`TestP1_4_SequentialInterrupts` proves two interrupts both fire;
`TestPendingInjectsFIFOOrdering` proves ordering under identical
timestamps. **Disclosed gap**: `TestP1_4_SequentialInterrupts` calls
`handleInterruptTick` directly rather than through the real ticker
goroutine, so it doesn't exercise the goroutine lifecycle itself — that
part is instead covered by the orchestrator's own direct reproduction of
a real deadlock in the ticker's join logic (see property 8).

### 3. Late checkpoint after terminal finish changes neither the SQLite row nor the last UI event; overlapping generations don't race

**Status: CLOSED.** `UpdateMessageIfNotTerminal` changed `:exec` →
`:execrows`; `message.Service.Update` skips the publish when
`rowsAffected == 0` (P0-2 part A, commit `fda2c19d`).
`checkpointGeneration` access is now under a dedicated mutex, and
per-writer coalescing state (`lastPartsLen`) is local instead of shared
(P0-2 part B, same commit). `TestLateCheckpointDoesNotPublishStalePartial`
checks the actual last broker event, not just the DB row — this was the
specific gap the previous round's version of this fix had.
**Disclosed gap**: the race-detector regression test
(`TestCheckpointGenerationOverlap`) drives two genuinely overlapping
generations through the real `interruptAndReplace` path, but its own
revert-check (mutex removed) did not reproduce a race under `-race` —
independently re-verified by the orchestrator, not just taken on the
delegate's word. The fix's correctness here rests on code-level mutual
exclusion (every access to `checkpointGeneration` is now under one mutex,
by construction), not on this test having caught a live race.

### 4. No accepted orphan call exists only in process memory; crash/shutdown leaves retryable or terminal durable state

**Status: CLOSED for the accept path; ONE FOLLOW-UP NEEDED (not blocking).**
`startBoundedDetachedRun` (in-memory-only, 30s-bounded) is removed
entirely; on durable-enqueue failure the call is now written to a new
`orphan_call_outbox` table, and if that write also fails the call is left
in an observable terminal-failure state with the error returned to the
caller (P0-3, commit `a039b41d`). `TestP0_3_OrphanOutboxDurability` /
`TestP0_3_OutboxWriteFailure` assert actual DB records, not "something ran".
**Follow-up filed, not blocking this gate**: nothing in production code
currently drains `orphan_call_outbox` (`run_queue_pump.go` was out of
scope for the P0-3 task) — entries accumulate with no active recovery
process yet. The property as stated ("durable state exists, not only in
memory") is satisfied; the property does NOT claim the durable state is
automatically recovered, only that it isn't lost. Recommend a follow-up
task to wire outbox draining into the pump.

### 5. Renewal hung longer than the safety budget cancels the executor BEFORE lease expiry; a stale owner can't persist without fencing

**Status: CLOSED, empirically verified.** An independent watchdog
goroutine tracks `lastSuccessfulRenewal` via an atomic and cancels
`execCtx` at `TTL - safety_margin` regardless of whether the renewal call
itself is stuck; each renewal's own DB timeout is now the remaining safe
budget instead of a fixed 30s (P1-1, commit `f036065f`). The old
now-unreachable fail-closed branch was removed as dead code rather than
left to rot alongside the new mechanism. Revert-check (watchdog goroutine
disabled) reproduced the predicted symptom (`coordinator did not observe
context cancellation within 8s`) — genuine, not simulated. Full
`internal/session` package confirmed green twice in isolation.

### 6. `401 → credential rotation → retry` uses the new provider client, doesn't create a second logical request

**Status: CLOSED at the code level; automated test coverage is WEAK
(disclosed, not hidden).** `runInternal` now builds a `rebuildCall`
callback that re-resolves session models and reconstructs the call
(preserving `LogicalCallID` and other logical fields) after a successful
credential refresh, before the retry (P1-2, commit `063b7c69`).
**Real bug found and fixed during this gate's own predecessor review**:
`runWithUnauthorizedRetry` called `rebuildCall()` unconditionally; the two
other call sites (summarize, sub-agent delegation) pass `nil` by design,
so any 401 that successfully refreshed credentials on those paths would
have panicked the coordinator — confirmed by direct reproduction (30s
timeout, goroutine dump), fixed with a nil check. **Test gap**:
`Test401Retry_RebuildsCallWithFreshCredentials`'s mock always succeeds on
the second call regardless of which client/token was actually used, so it
proves the rebuild-and-retry flow completes without error but does not
prove client identity switched. No dedicated fake-client-with-identity
test was built (would need a fake provider client that tracks which token
constructed it). Confidence in this property rests on the code path being
read and traced end-to-end (`rebuildCall` → `resolveSessionModels` →
`pinned.pin(&newCall)` → `*trackCall = newCall`), not on empirical proof.

### 7. Concurrent config reload can't mix models/providers/prompt-prefix across generations or save the mixed result under a new cache key

**Status: CLOSED at the code level; automated test coverage is WEAK
(disclosed, not hidden).** `ConfigStore.Snapshot() (*Config, uint64)`
returns both atomically from one `storeSnapshot` load; threaded through
`resolveSessionModels`, `buildModelsFromCfg` (now takes `cfg` as a
parameter instead of re-reading it), `applyModelOverrides`,
`RebuildSessionAgentCall`, `Model()` (P1-3, commit `063b7c69`).
`SetProviderRuntimeConfig` now goes through `publishLocked` with a proper
generation bump instead of mutating the Providers map in place without
incrementing generation — this specific defect (generation never
incrementing on this path) is fixed and verified
(`TestSetProviderRuntimeConfig_IncrementsGeneration`, revert-checked:
reverting to in-place mutation makes the test fail as predicted).
**Test gap**: no test exercises a real concurrent reload landing between
`resolveSessionModels`'s reads — `TestSnapshot_Atomicity` calls
`Snapshot()` serially 100 times with nothing changing config in between,
which is not a meaningful stress of the atomicity claim. Confidence rests
on the `Snapshot()` API genuinely returning one struct's two fields from
one atomic pointer load (verified by reading `loadSnapshot()`) plus every
call site now going through it instead of separate `Config()`/
`Generation()` calls (verified site-by-site during review).

### 8. `CancelAll`/`Shutdown` joins interrupt/checkpoint/pump writers, or explicitly returns forced state, without closing their resources out from under them

**Status: CLOSED, with a real deadlock found and fixed along the way.**
The interrupt ticker now returns a `done` channel; call sites join it
before returning (P1-4, commit `3ee3501d`). Checkpoint writes have both
`cancel` and a 30s deadline, and `stopCheckpoint` cancels the write
immediately rather than waiting out its own grace first (P0-2, prior
round + this round's mutex fix). `RunQueuePump.Stop` bounds both workers
and the main loop with one 5s deadline (prior round, still intact —
confirmed by this round's `internal/session` suite passing).
**Found and fixed during this gate's own predecessor review, not by any
delegate**: both interrupt-ticker call sites wrote two separate `defer`
statements (cancel first in source order, join second) — Go defers run
LIFO, so the join executed BEFORE the cancel, meaning every call to
`runInternal`/`RunSessionAgentCall` waited forever for a ticker goroutine
that was never told to stop. Reproduced with a 30s-bounded isolated test
run (goroutine dump confirmed the ticker parked in its `select`), fixed
by combining cancel+join into one correctly-ordered deferred closure.
This is exactly the class of defect the review's whole "who joins whom"
line of findings (P1-4, and the design doc's cancellation-authority
analysis) was worried about, and it would not have been caught by either
delegate's own test suite — both new regression tests exercise the
correct order deliberately rather than the actual buggy call sites.

### 9. Production INFO/ERROR logs contain no raw prompt, history, attachment content, or auth material

**Status: CLOSED, empirically verified.** The new orphan-outbox
ERROR/WARN logging uses only session/logical IDs, prompt length, and a
SHA-256 prefix hash by default; raw prompt is opt-in via the same
`CRUSH_CLIPROVIDER_LOG_RAW_PROMPT` guard already used by the CLI provider
(P0-3 part B / SEC-1, commit `a039b41d`). **Verified, not assumed**: the
original delegate's test asserted nothing about actual log output (its
own comment claimed "we can't easily capture slog output," which is
false — this exact repo already has the pattern in
`cliprovider/provider_security_test.go`). Rewritten to capture real
`slog` output via `slog.SetDefault(slog.New(slog.NewTextHandler(&buf,
...)))`; independently revert-checked by the orchestrator (temporarily
reintroducing raw-prompt logging makes the test fail with the secret
marker present in the captured log — reproduced directly, not taken on
faith).

## Cross-cutting notes

- **Two bugs were found and fixed by the orchestrator's own zero-trust
  verification, not by any delegate**: the `runWithUnauthorizedRetry` nil
  panic (property 6) and the interrupt-ticker LIFO defer deadlock
  (property 8). Both are the kind of defect that a delegate's own
  "tests pass" claim would not surface — they required independently
  reproducing the failure with a bounded timeout and reading a goroutine
  dump, not trusting a green checkmark.
- **A pre-existing, unrelated break was found and fixed**:
  `internal/app`'s `mockSessionService` never implemented the 6
  orphan-outbox methods `session.Service` gained in P0-3, silently
  breaking `go vet`/`go test` (not `go build`) on `internal/app` since
  that merge — a gap in P0-3's own verification, which only ran
  `./internal/agent/... ./internal/session/... ./internal/db/...` per its
  scope. Fixed in commit `0140b2ac`.
- **A pre-existing gofmt gap was found and fixed**: files touched by the
  P1-1 fix-back were not run through `gofmt -w` before merge (struct
  alignment, missing trailing newline). Fixed in commit `7c9af748`.
- **Known, consistently-reproduced pre-existing flaky tests** (confirmed
  via repeated isolated re-runs during this round, unrelated to any of
  this round's diffs): `TestP0_338_FinalizerReachableDespiteHungCleanup`,
  `TestBashTool_CtxCancelWaitsForConfirmedProcessKill`,
  `TestReleaseGate_9_DoubleFailureNoDuplicate`,
  `TestReleaseGate_P0_2_GenuineFailureStillExhaustsAfterMaxAttempts`,
  `TestP1_2_ExecCtxCanceledOnLeaseLoss`,
  `TestP1_1_FastRenewalNoFalsePositive` (20/20 clean in isolation),
  `TestP1_4_BoundedWorkerPoolRespectsLimit` (10/10 clean in isolation) —
  all timing-margin-sensitive tests that occasionally lose a race against
  Windows scheduling jitter under this machine's full-package `-race`
  load, and all pass reliably alone. None of these are new to this round.

## Verdict

**GO**, with one explicit, non-blocking follow-up:

1. File a follow-up task to wire `orphan_call_outbox` draining into
   `run_queue_pump.go` (property 4's recovery half — the durability half
   is closed).

Properties 6 and 7 are closed at the code level but rest on manual
code-path verification rather than a fully empirical test for the
specific "old vs. new" / "torn read" scenario each describes — this is
disclosed above per-property, not hidden. Both mechanisms (`rebuildCall`,
`Snapshot()`) are structurally simple enough (a callback invoked
correctly, one atomic pointer load instead of three) that the residual
risk is judged low, but a future round that has more time to build the
harnesses described above (fake client with tracked identity; a real
concurrent-reload race, not a serial 100x smoke test) would close the gap
completely.

The originally-proposed P2-4 state-machine unification refactor
(`docs/design/2026-08-12-state-machine-unification.md`,
`docs/plans/2026-08-12-state-machine-unification-plan.md`) remains
explicitly NOT started and NOT required for this gate, per that design
doc's own recommendation — this round's point fixes closed all 9
properties without it.
