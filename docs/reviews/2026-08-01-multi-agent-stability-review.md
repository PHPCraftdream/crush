# Multi-agent stability review

Date: 2026-08-01  
Reviewed range: `66c4d062..e9544a8f` (2026-07-18 through 2026-08-01)  
Mode: read-only static review; tests and builds were not run.

## Release recommendation

Do not release the current `e9544a8f` state yet. The reviewed code contains
two release blockers and several high-priority lifecycle, cancellation, input
handling, and process-safety problems.

Recommended stabilization order:

1. Refactor the `sessionAgent.Run` lifecycle, queue drain, and session
   ownership.
2. Bound title generation and every goroutine awaited by `Run`.
3. Make child-cost finalization independent of the canceled turn context.
4. Verify real OS-lock ownership before `sessions kill` terminates a PID.
5. Preserve slow or partial piped stdin.
6. Separate parent-delegation and child-tool timeout contracts.
7. Complete hard-cap semantics and make explicit `--agents single` win.

## Release blockers

### 1. Recursive `Run` starts before the previous call releases its OS lock

Relevant code:

- `internal/agent/agent.go:455-459`: non-atomic busy check and queueing.
- `internal/agent/agent.go:489-519`: OS-level session lock acquisition and
  deferred release.
- `internal/agent/agent.go:619-620`: the active request is registered only
  after lock acquisition, model setup, and the DB preamble.
- `internal/agent/agent.go:1794-1804`: canceled-turn queue drain recursively
  calls `Run`.
- `internal/agent/agent.go:1825-1844`: normal queue drain recursively calls
  `Run`.
- `internal/agent/agent.go:2035-2048`: summarization queue drain also calls
  `Run`.

The queue paths delete the in-memory active-request entry and call
`a.Run(...)` recursively. Go evaluates the recursive call before running the
current function's deferred cleanup. The previous invocation therefore still
holds its `SessionLock` and still owns other per-turn resources when the next
invocation attempts to acquire the same lock.

This can make interrupt-and-send, queued prompts, resumptions, and post-compact
continuations fail with `session already in use` against their own previous
turn. It also overlaps watchdog, title-generation, and cleanup lifetimes.

There is an additional entry race: `IsSessionBusy` and
`activeRequests.Set` are not one atomic operation. Two goroutines can both
observe an idle session before either registers itself. The OS lock then turns
an intended same-process queue operation into an error, with platform-specific
timing.

The interaction was exposed by the recent lock work. Commit `18055ba0` extended
the inter-process lock to sub-agents and stated that same-process re-entry would
be caught by `IsSessionBusy`, but the recursive queue drain explicitly deletes
that busy state before re-entering while the OS lock remains held.

Recommended fix:

- Replace recursive continuation with an outer dispatcher loop.
- Extract a `runOne`-style operation that returns its result and optional next
  call.
- Let every deferred cleanup and lock release finish before starting the next
  turn.
- Add an atomic session reservation operation at the very beginning of `Run`;
  the losing caller must append to the queue.
- Treat regular runs and manual summarization as states of the same session
  ownership machine instead of unrelated map keys.

Required regressions:

- Canceled `Run` with a queued message and a real session lock.
- Successful `Run` with a queued message and a real session lock.
- Two concurrent `Run` calls for the same session ID.
- Queued continuation after manual and automatic summarization.

### 2. Unbounded title generation can keep `Run` stuck forever

Relevant code:

- `internal/agent/agent.go:581-598`: title goroutine uses the original caller
  context and `Run` defers an unconditional `wg.Wait`.
- `internal/agent/agent.go:619-683`: the main turn receives a separate
  `genCtx` and watchdog only after title generation has started.
- `internal/agent/agent.go:2470-2488`: only the fallback rename has a bounded
  detached context.
- `internal/agent/agent.go:2534-2545`: small and large model title streams have
  no title-specific timeout or watchdog.

Title generation uses `titleCtx := ctx`, not the main turn's `genCtx`. If the
title provider stream hangs and the caller supplied no deadline, the main
watchdog may correctly cancel the turn but `Run` will still wait forever in
`wg.Wait`.

This also weakens the DB preamble fix in `e9544a8f`: a preamble call can time
out after 60 seconds, but returning from `Run` still waits for a title goroutine
that may never finish. A best-effort metadata operation is therefore able to
defeat every primary timeout contract.

Recommended fix:

- Give title generation its own bounded context.
- Tie it to turn cancellation while retaining a short, bounded detached
  context only for the final fallback write.
- Do not perform an unbounded `Wait` for a best-effort operation.
- Prefer a structured goroutine lifecycle where every awaited sibling has an
  explicit deadline.

Required regression:

- A title provider whose stream never returns while the main turn or preamble
  finishes with an error. `Run` must return within the configured bound.

## High-priority findings

### 3. Child cost transfer uses the already-canceled turn context

Relevant code:

- `internal/agent/coordinator.go:2470-2489`: child execution uses `ctx`.
- `internal/agent/coordinator.go:2497-2506`: cost transfer immediately reuses
  the same `ctx` after the child returns.
- `internal/session/session.go:586-627`: transfer begins and commits a database
  transaction with that context.

When a child returns because the user, the parent watchdog, or an outer timeout
canceled `ctx`, `TransferChildCostToParent` receives an already-canceled
context. `BeginTx` or the first query fails immediately.

The transactional ledger added in `1481be78` keeps the delta idempotent, but its
comment assumes a later successful call will recover it. A failed child may
never be resumed, and a parent retry may create a different child session. In
that case the parent permanently under-reports the original child's cost.

Recommended fix:

- After child completion, transfer cost with a short bounded context derived
  from `context.WithoutCancel(ctx)`.
- Keep the current transactional and idempotent ledger behavior.
- Use a similarly bounded detached context for best-effort post-commit refresh
  publication if needed.

Required regression:

- Persist child usage, cancel the run context, return from the child, and prove
  that the parent is still charged exactly once.

### 4. `sessions kill` can terminate an unrelated process from stale PID data

Relevant code:

- `internal/session/lock.go:177-182`: PID is written to the lock file and
  sidecar.
- `internal/session/lock.go:200-221`: normal release unlocks and closes the
  file but does not clear either PID record.
- `internal/session/lock.go:367-423`: reads prefer the unlocked `.pid` sidecar.
- `internal/cmd/sessions_kill.go:61-83`: command reads the saved PID and kills
  it without proving that the OS lock is currently held by that process.
- `internal/cmd/sessions_kill.go:122-145`: cleanup removes only the primary
  lock file.

Completed sessions leave stale lock metadata. Operating systems eventually
reuse PIDs. Calling `sessions kill` for an old session can therefore terminate
an unrelated process that happens to own the recycled PID.

Commit `cdd1f5ca` made the live PID readable on Windows via a sidecar, but it
did not bind the PID to the current lock generation. It also creates a window
where a new holder has acquired the OS lock but has not yet replaced a stale
sidecar, so a contender can observe the wrong holder PID.

Recommended fix:

- Before killing anything, attempt a non-blocking OS lock on the exact lock
  file.
- If the lock can be acquired, no active owner exists: do not kill the stored
  PID; clear stale metadata instead.
- On normal release, clear the primary PID and sidecar while still holding the
  OS lock, then unlock.
- Remove or clear both primary and sidecar metadata in verified cleanup paths.
- Consider recording a process start timestamp or lock generation along with
  the PID for additional validation.

Required regressions:

- Released lock whose PID now belongs to a live unrelated helper process.
- Actively held lock with a matching sidecar.
- Stale sidecar during acquisition of a new lock generation.

### 5. The named-pipe timeout silently discards real stdin

Relevant code:

- `internal/cmd/root.go:362-376`: fixed three-second grace period.
- `internal/cmd/root.go:394-417`: named pipe is consumed with `io.ReadAll` in a
  goroutine.
- `internal/cmd/root.go:418-430`: timeout returns the original prompt and
  discards the pipe.

The timer measures time to EOF, not time to the first byte. A producer can
write data immediately but keep the pipe open for more than three seconds. At
the deadline `io.ReadAll` has not returned, so Crush drops all bytes already
read, logs that the pipe produced no data, and proceeds with only the
positional prompt. The reader goroutine remains alive and continues consuming
the pipe.

This is silent input loss for slow producers, large streams, and pipelines that
intentionally remain open while generating content.

Recommended fix:

- Bound only the wait for the first byte.
- Once input starts arriving, preserve it and continue reading.
- If a continuing stream must also be bounded, use an idle timeout and return
  the accumulated bytes with an explicit warning or error. Never silently
  discard bytes that were already received.

Required regression:

- Pipe writes an initial chunk immediately, waits longer than
  `stdinReadGrace`, writes another chunk, and closes. The prompt must retain
  the input according to the documented completion or idle-timeout policy.

### 6. A single watchdog cap creates a parent/child cancellation race

Relevant code:

- `internal/agent/agent.go:632-671`: every `Run` resolves the same tool cap.
- `internal/agent/stream_watchdog.go:183-223`: the cap bounds every tool batch,
  including an `agent` delegation.

Commit `e9544a8f` replaced separate primitive-tool and delegation limits with
one 45-minute default and removed the ability to distinguish tool kinds. A
parent's `agent` tool timer and the child's own turn/tool timers now receive
the same cap and start at nearly the same time. The parent starts slightly
earlier, so it can cancel the whole child call before the child has a chance to
produce its own bounded result and finish cleanup. A small explicit
`StreamToolTimeoutSeconds` makes this race easier to hit.

The previous unlimited-delegation design was unsafe, but one scalar still does
not represent three different contracts: a primitive tool, a whole nested
conversation, and a primitive tool inside that conversation.

Recommended fix:

- Keep a finite absolute parent-delegation cap.
- Let the child enforce its own stream and primitive-tool limits.
- Make the parent delegation cap greater than the child's bound plus a cleanup
  grace period, or track delegation progress/heartbeat explicitly.
- Ensure parent cancellation leaves enough bounded time for finish persistence
  and cost transfer.

## Medium-priority findings

### 7. `--timeout-hard-cap` is not hard without progress extension

Relevant code:

- `internal/agent/stream_watchdog.go:212-223`: hard cap is checked while tools
  are in flight.
- `internal/agent/stream_watchdog.go:228-260`: outside tools, the hard deadline
  is checked only inside `if extendsOnProgress`.
- `internal/cmd/run.go:760`: CLI describes it as a maximum wall-clock time.

With `hardCap > 0`, `extendsOnProgress == false`, and continuous provider
callbacks, `bump` continually resets idle activity. The normal idle branch
never fires, and the hard deadline is never consulted. The turn can exceed the
operator's explicit wall-clock maximum.

Recommended fix:

- Check the unconditional wall-clock hard deadline before the
  `extendsOnProgress` branch.
- Preserve the separate `toolTimeout` classification when the deadline fires
  during tool execution.

Required regression:

- Configure a hard cap without progress extension and continuously call
  `bump`; the watchdog must still fire at the cap.

### 8. Explicit `--agents single` is ignored for smart role with Worker

Relevant code:

- `internal/cmd/run.go:490-506`: unset and explicit `single` both set
  `DisableSubAgents` and promise removal of delegation tools.
- `internal/app/app.go:1013-1035`: smart role with a configured Worker restores
  the `agent` tool even when `single` was explicit.

This behavior was deliberately added in `18f802cd`, but it contradicts the CLI
contract and removes the operator's explicit opt-out from multi-agent work.
Automatic orchestration can be a reasonable default, but an explicit
`--agents single` should take precedence.

Recommended fix:

- Preserve the distinction between an unset agents policy and explicit
  `single` in `RunOverrides`.
- Bypass the default ban only when the policy was unset; never override an
  explicit operator choice.

## Regression coverage gaps

The current tests cover lock primitives, generic cost-transfer errors, an
immediate pipe EOF, a pipe that never writes, and hard cap with progress
extension. They do not cover the integration boundaries identified above:

- Queue drain through a real `Run` while its OS lock is held.
- Atomic same-session ownership under two simultaneous calls.
- A title provider that never returns.
- Cost finalization after context cancellation.
- Stale or recycled PID safety in `sessions kill`.
- Slow or partial named-pipe input.
- Hard cap when `extendsOnProgress` is disabled.
- Explicit `--agents single` with smart role and Worker configured.

There is also a test-isolation problem in
`internal/agent/run_preamble_timeout_test.go:43-48`: the test calls
`t.Parallel()` and mutates the package-global `sessionPreambleMaxDuration`.
That can race with any other parallel test entering `Run`, causing flaky
timing and race-detector failures.

Recommended fix:

- Inject the preamble timeout through `SessionAgentOptions` or an agent field.
- Avoid mutable package globals in parallel tests.

## Lower-priority diagnostic mismatch

`internal/agent/stream_watchdog.go:56-58` says `onFire` runs before
`cancel`, but every firing branch calls `cancel()` first and `onFire` second.
The current callback starts by dumping goroutines, so cancellation can begin
tearing down the evidence before the dump is captured. Either document the
actual post-cancel order or capture a lightweight diagnostic snapshot before
cancellation while ensuring diagnostics can never delay cancellation
unboundedly.

## Conclusion

The central stability issue is not an individual timeout value. Session
ownership, queued continuation, summarization, OS locking, watchdog lifetime,
and detached finalization are currently implemented as partially independent
mechanisms. Recent fixes strengthened each mechanism locally but exposed
conflicts at their boundaries.

The stable shape should have one explicit per-session state machine:

- atomically reserve the session;
- acquire the OS lock;
- execute exactly one bounded turn;
- finish all required persistence and bounded side work;
- release watchdogs, goroutines, in-memory ownership, and OS lock;
- only then dispatch the next queued turn.

Until that lifecycle is enforced and the blocker regressions exist, additional
local timeout and lock fixes are likely to continue moving the failure from one
boundary to another.
