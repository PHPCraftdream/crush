# Per-session owner/mailbox design (task #278)

Status: **design only, no code**. Written in response to
`docs/reviews/2026-08-04-multi-agent-stability-follow-up.md`, specifically
P0-2, P0-3, P1-1, P1-2, and the "Рекомендуемая последовательность
исправлений" item 1 ("Ввести единый per-session owner/mailbox с атомарными
операциями: `submit`, `interrupt-and-replace`, `inject`, `compact`,
`drain-or-release`").

This document proposes replacing five independently-mutated pieces of state
in `internal/agent/agent.go` —

- `activeRequests *csync.Map[string, context.CancelFunc]`
- `messageQueue *csync.KeyedQueue[SessionAgentCall]`
- `injectQueue *csync.KeyedQueue[message.Message]`
- the `sessionID+"-summarize"` synthetic key inside `activeRequests`
- `sessionStartMu sync.Mutex` (the reservation gate in `tryReserveSession`)

— with one per-session state machine (the **mailbox**) that makes every
cross-structure handoff a single critical section instead of a sequence of
independently-locked operations.

Section numbers below match the eight numbered requirements in the task
prompt.

---

## 0. Why this has to be one structure, not five patched ones

Every P0/P1 bug in the follow-up review has the identical shape: state A is
mutated, then — as a **separate**, non-atomic step — state B is mutated in
response, and a third actor can observe the gap between those two steps.

| Bug | Step 1 | Step 2 | Actor that lands in the gap |
|---|---|---|---|
| P0-2 | `messageQueue.Append` (coordinator) | `Cancel` → `messageQueue.Clear` (agent) | nobody external — it's the **same call site**, self-inflicted, 100% reproducible |
| P0-3 | owner's final `messageQueue.PopFront` returns empty | `activeRequests.Del` (deferred, in `Run`) | a concurrent `Run` call for the same session |
| P1-1 | `createUserMessage` (DB write) | `injectQueue.Append` (conditional on `IsSessionBusy`) | the in-flight turn's own `PrepareStep`, which may read the DB copy before or the queue copy after, or both |
| P1-2 | replacement durably queued | `runCtx` (spans the *whole* `Run` call, not just one turn) gets cancelled during DB preamble | the turn loop itself — `hasNext=false` short-circuits before checking the mailbox |

Four different pairs of steps, four different second actors, one missing
primitive: **an atomic "mutate the mailbox and decide who's responsible for
draining it" transition.** Patching each bug locally (a new `if` guarding
`Cancel`'s clear, a recheck after `PopFront`, a dedup flag on inject, a
second context) would each close today's specific window while leaving the
underlying shape — N independently-locked structures glued together by
happens-before assumptions — in place for the next feature to reopen a
sibling window in. This is explicitly called out in the review's closing
section and is the reason for doing this design first.

---

## 1. Mailbox state — exactly what it holds

One mailbox per session id, created lazily on first touch and never deleted
(matching `csync.Map`'s existing all-sessions-forever lifetime — deletion
policy is unchanged by this design). Conceptually:

```go
type mailboxState int

const (
    mbIdle       mailboxState = iota // no owner, nothing queued
    mbOwned                          // a turn loop holds ownership
    mbReleasing                      // owner is mid drain-or-release transition (see §3)
)

type generation struct {
    id     uint64             // monotonic per session, bumped every turn AND every preamble
    cancel context.CancelFunc // cancels ONLY this generation (see §4 for why not runCtx)
}

type mailbox struct {
    mu sync.Mutex // single critical section for ALL fields below

    state mailboxState

    // dispatcher is the durable, call-scoped cancel func — spans the whole
    // Run() call (every turn + every preamble), analogous to today's
    // runCancel registered by tryReserveSession. It is NEVER the thing an
    // interrupt cancels (see §4); it exists so CancelAll / process shutdown
    // can still tear down a session that has no live generation at the
    // instant of the call (e.g. between turns).
    dispatcherCancel context.CancelFunc

    // current is the active generation's id+cancel, or zero value when
    // state != mbOwned. Interrupt/Cancel target THIS, never dispatcherCancel.
    current generation

    // submitted holds at most one pending SessionAgentCall submitted while
    // owned — replaces messageQueue for the "queue a normal follow-up"
    // case. A plain queue of depth 1 is enough because Run's turn loop
    // always fully drains it before releasing (§3); nothing in the current
    // code path enqueues more than one call ahead in practice, but see the
    // migration note in §7 about preserving multi-item FIFO semantics.
    submitted []SessionAgentCall

    // replacement, when non-nil, is an interrupt-and-replace payload that
    // must be consumed by the NEXT generation the owner starts, and the
    // CURRENT generation must be cancelled to make room for it. Distinct
    // from `submitted` because it carries an explicit "cancel current, then
    // run me next" semantic instead of "run me after current finishes".
    replacement *SessionAgentCall

    // injects holds messages already persisted to the DB, waiting to be
    // spliced into prepared.Messages by the owner's PrepareStep. Each entry
    // carries the generation id it was submitted against so drain (§5) can
    // decide "did the owner's DB read already include this row, or not".
    injects []pendingInject

    // compact holds at most one pending manual-compact request (opts +
    // requestedAt). Present only so a compact submitted while owned is
    // remembered until drain-or-release; see §6 for why this field is not
    // exercised by this task.
    compact *fantasy.ProviderOptions
}

type pendingInject struct {
    msg          message.Message
    afterGenID   uint64 // generation id current AT THE MOMENT InjectMessage ran
}
```

All seven `mailbox` methods below (`submit`, `interruptAndReplace`,
`inject`, `compact`, `beginGeneration`, `drainOrRelease`,
`cancelCurrentGeneration`) take `mu` for their entire body — no method calls
another mailbox method (to avoid self-deadlock on a non-reentrant mutex);
shared logic is factored into unexported, lock-already-held helpers.

This mailbox is a value owned by `sessionAgent`, stored in a
`csync.Map[string, *mailbox]` keyed by session id — structurally the same
container shape as today's `activeRequests`, so the "one map, lazily
populated, entries never explicitly deleted" lifetime story does not change.

---

## 2. Mapping the five old structures onto the mailbox

| Old structure | Replaced by | Notes |
|---|---|---|
| `activeRequests.Get/Set(sessionID, cancel)` (the busy/reservation slot) | `mailbox.state != mbIdle` (busy check) + `mailbox.dispatcherCancel`/`mailbox.current.cancel` (cancel targets) | `IsSessionBusy(id)` becomes `mailbox.state != mbIdle`, O(1), same semantics observers rely on today. |
| `activeRequests.Get/Set(sessionID+"-summarize", cancel)` | `mailbox.compact` field + a SEPARATE, still-independent cancel scope (see §6) | The synthetic string key goes away entirely — no more risk of one defer deleting the other's registration (P0-4's root cause, out of scope for #278 but the mailbox shape must not block fixing it later). |
| `messageQueue.Append/TakeAll/PopFront/Clear(sessionID)` | `mailbox.submitted` (drained in `beginGeneration`, appended in `submit`) | Same FIFO contract, now guarded by the SAME mutex as the state transition that decides whether to drain it or become new owner (§3). |
| `injectQueue.Append/TakeAll(sessionID)` | `mailbox.injects` (drained in `beginGeneration`'s `PrepareStep`, deduplicated by `afterGenID`, see §5) | |
| `sessionStartMu sync.Mutex` (the Run()-entry gate) | the mailbox's own `mu`, taken once per `submit`/`interruptAndReplace` call — no separate package-level mutex needed, because ownership decisions now happen INSIDE the same critical section as the queue mutation they used to race against | Removes the two-mutex hazard (`sessionStartMu` + `activeRequests`' internal lock) the current code has to reason about together. |

`sessionAgent` keeps a `csync.Map[string, *mailbox]` in place of the three
maps/queues; no other exported field changes shape (see §8).

---

## 3. Atomic "final drain → release" (closes P0-3)

Today: owner calls `messageQueue.PopFront`, sees empty, returns
`hasNext=false`; `Run`'s `defer releaseSessionReservation` runs strictly
*after* that return, so there is a window — between the empty `PopFront`
and the deferred `activeRequests.Del` — where a concurrent submit can land,
see "busy", queue itself, and get silently orphaned forever (no owner is
looking at `submitted` anymore).

**New `drainOrRelease(sessionID string) (SessionAgentCall, bool)`** — called
by the owner (agent-internal, replacing the `messageQueue.PopFront` call at
the end of `runTurn` AND the one in the cancel-drain branch AND the one in
`runSummarizeCore`) at the exact point today's code calls `PopFront`:

```go
func (mb *mailbox) drainOrRelease() (SessionAgentCall, bool) {
    mb.mu.Lock()
    defer mb.mu.Unlock()

    if len(mb.submitted) > 0 {
        next := mb.submitted[0]
        mb.submitted = mb.submitted[1:]
        return next, true // caller runs another turn; state stays mbOwned
    }
    // Nothing queued AT THE INSTANT OF THIS CHECK, and — because mu is
    // held — nothing CAN be queued between this check and the state flip
    // below. This is the whole fix: today's bug is that the "queue is
    // empty" observation and the "release ownership" mutation are two
    // separate critical sections; here they are one.
    mb.state = mbIdle
    mb.current = generation{}
    mb.dispatcherCancel = nil
    return SessionAgentCall{}, false
}
```

And **`submit(sessionID string, call SessionAgentCall, dispatcherCancel
context.CancelFunc) (becomeOwner bool)`** — replaces both
`tryReserveSession`+`activeRequests.Set` (the "am I the new owner" path) and
`messageQueue.Append` (the "queue behind the current owner" path) as ONE
function:

```go
func (mb *mailbox) submit(call SessionAgentCall, dispatcherCancel context.CancelFunc) bool {
    mb.mu.Lock()
    defer mb.mu.Unlock()

    if mb.state == mbIdle {
        mb.state = mbOwned
        mb.dispatcherCancel = dispatcherCancel
        return true // caller (Run) becomes the new owner, runs call itself
    }
    mb.submitted = append(mb.submitted, call)
    return false // caller queues and returns nil, exactly like today
}
```

Because `submit` and `drainOrRelease` share the same `mb.mu`, there is no
interleaving in which `drainOrRelease` observes `submitted == nil` AND
`submit` appends to it before the state flips to idle — the two are
strictly ordered by the mutex. A concurrent `submit` that arrives
microseconds after `drainOrRelease` released the lock at `mbIdle` correctly
becomes the new owner (`state == mbIdle` → `true`); one that arrives a
microsecond earlier is correctly appended to `submitted` and picked up by
the SAME `drainOrRelease` call that is currently holding the lock (it can't
have released yet). No message can land in the gap because there is no
gap: the state that answers "is there an owner" and the queue that holds
"what's waiting" are the same lock's data.

This directly satisfies release-gate scenario 3 ("Обычный concurrent send
точно в окне `final drain -> release` либо становится owner, либо
гарантированно исполняется старым owner").

---

## 4. Atomic "interrupt and replace" (closes P0-2 and P1-2)

### P0-2's exact defect, restated

`InterruptAndSend` does `QueueMessage(call)` then `Cancel(sessionID)`, and
`Cancel` unconditionally clears `messageQueue`. Two sequential, independently-
locked mutations; the second one wipes the first one's effect. Not a race —
deterministic self-destruction on every call.

### P1-2's exact defect, restated

Even with P0-2 fixed by "don't clear on cancel", a single `runCtx` spans
the whole `Run()` loop (every turn's preamble derives its `genCtx` from
the same `runCtx`, and `runCtx` IS what `tryReserveSession` registers as
the cancel target during preamble). If interrupt cancels `runCtx` while
inside `sessionAgent.sessions.Get`/`getSessionMessages`/`createUserMessage`
(the DB preamble, before `runTurn` has swapped in a fresh per-turn
`genCtx`), the *entire outer context* — including everything a NEXT turn
would derive its own context from — is now permanently canceled.
`runTurn`'s preamble read fails, returns `hasNext=false` immediately, and
`Run` exits without ever looking at the replacement, because there is no
живой context left to run another turn on.

### The fix: two-tier context, one atomic mutation

The mailbox already separates `dispatcherCancel` (call-scoped, lives for
the whole `Run()` invocation) from `current.cancel` (generation-scoped,
lives for exactly one preamble+turn). This split is not cosmetic — it is
the mechanism that makes interrupt-during-preamble recoverable:

- `dispatcherCancel` cancels the **whole dispatcher** — used only by
  `CancelAll`/process shutdown, where "kill this session's Run call
  entirely, no more turns" is genuinely the desired outcome.
- `current.cancel` cancels **only the in-flight generation** (preamble OR
  stream, whichever is active when interrupt fires) — this is what every
  interrupt, watchdog fire, and mid-turn `/compact` targets. Cancelling it
  never touches `dispatcherCancel`'s context, so the turn LOOP itself
  (the `for` in `Run`, driven by `dispatcherCancel`'s parent context) is
  still alive and able to start a fresh generation immediately after.

**`interruptAndReplace(sessionID string, call SessionAgentCall) (cancelFn
context.CancelFunc, hadOwner bool)`** — the coordinator's new single
entry point, replacing `QueueMessage`+`Cancel` as one mailbox operation:

```go
func (mb *mailbox) interruptAndReplace(call SessionAgentCall) (context.CancelFunc, bool) {
    mb.mu.Lock()
    defer mb.mu.Unlock()

    if mb.state != mbOwned {
        // Nobody running: behave like a plain submit that also happens to
        // return "no cancel needed" — the caller (coordinator) then starts
        // a fresh Run() with call directly instead of relying on drain.
        return nil, false
    }
    // Durably record the replacement FIRST, under the same lock that is
    // about to cancel the current generation. There is no window between
    // "replacement is recorded" and "current generation is cancelled" for
    // an external observer to land in, because both happen before mu is
    // released.
    mb.replacement = &call
    cancel := mb.current.cancel
    return cancel, true
}
```

The owner side (inside `runTurn`'s cancel-handling branch, replacing
today's `messageQueue.PopFront` check at the `isCancelErr` site) calls a
**generation-aware drain that checks `replacement` before `submitted`**:

```go
func (mb *mailbox) drainAfterCancel() (SessionAgentCall, bool) {
    mb.mu.Lock()
    defer mb.mu.Unlock()

    if mb.replacement != nil {
        next := *mb.replacement
        mb.replacement = nil
        return next, true
    }
    if len(mb.submitted) > 0 {
        next := mb.submitted[0]
        mb.submitted = mb.submitted[1:]
        return next, true
    }
    return SessionAgentCall{}, false
}
```

Because `mb.replacement` is set INSIDE the same critical section that reads
`mb.current.cancel`, and `current.cancel` is scoped to one generation (not
the whole dispatcher), the sequence is:

1. `interruptAndReplace` — under `mu` — writes `replacement` and returns
   the CURRENT generation's cancel func.
2. Coordinator calls that cancel func (outside `mu` — cancelling a context
   never needs the mailbox lock).
3. Whatever the current generation was doing — mid-stream OR mid-preamble
   — observes its own `genCtx.Done()` and unwinds. Critically, unwinding
   a generation-scoped context does **not** cancel `dispatcherCancel`'s
   context, so `Run`'s outer loop is untouched and about to ask the
   mailbox "what's next" via `drainAfterCancel`.
4. `drainAfterCancel` — under `mu` again — finds `replacement` still there
   (nothing could have cleared it: only `drainAfterCancel` itself clears
   it, and only one owner thread calls it) and hands it back as the next
   turn's call.

This closes P0-2 (the replacement is never wiped — there is no code path
left that clears `submitted`/`replacement` as a side effect of cancelling)
AND P1-2 (cancelling mid-preamble only kills that one generation's context;
the dispatcher-level context that the turn loop itself runs on is
untouched, so `runTurn` returning early from a cancelled preamble is
followed by the loop's next iteration calling `beginGeneration` again
— a fresh generation, fresh `genCtx`, immediately able to consume
`replacement` via `drainAfterCancel`). This requires `runTurn`'s preamble
failure path to route through `drainAfterCancel` instead of returning
`hasNext=false` unconditionally — a real code change, but one this design
explicitly authorizes ahead of implementation since it's the crux of P1-2's
fix (see §7, non-incremental cutover boundary).

`Cancel(sessionID)` (the plain interrupt-with-no-replacement path used by
Ctrl-C / `sessions kill` / a bare stop button) becomes: cancel
`mb.current.cancel` only, and **do not touch `submitted`/`injects` at
all** — those represent durable user intent to run more turns and a plain
cancel was never supposed to discard them either (this was arguably always
a latent second bug riding along with P0-2: today's `Cancel` clears
`messageQueue`/`injectQueue` unconditionally even for a bare abort with no
replacement, silently dropping anything a caller had queued moments
earlier via `QueueMessage` for unrelated reasons). `ClearQueue` remains the
one explicit, intentional "drop everything queued" operation, now
implemented as a mailbox method that clears `submitted`/`replacement`/
`injects` together under `mu`.

---

## 5. Atomic inject dedup (closes P1-1)

### The exact defect, restated

`InjectMessage` does, as three separate steps: (1) `createUserMessage` —
DB write, immediately visible to any DB reader including the in-flight
turn's own history reload; (2) `IsSessionBusy` check; (3) conditionally
`injectQueue.Append`. If the in-flight turn's `PrepareStep` already read
history (step 1's row included) between steps 1 and 3, the row is now
BOTH in the turn's own message list AND in `injectQueue` — the turn's
`PrepareStep` drains `injectQueue` unconditionally and appends it a second
time. If instead the inject lands after the turn's *last* `PrepareStep`
call but before release, the row sits in `injectQueue` with no owner left
to drain it (queue-side version of P0-3), and the row is ALSO already in
the DB, so the *next* `Run` picks it up from DB history and then ALSO
drains the stale queue entry — same message twice, different failure
shape.

### The fix: generation-stamped injects, checked against a DB-read watermark

Every generation gets a monotonically increasing id (`generation.id` in
§1), incremented by `beginGeneration` (see below) each time a NEW
generation starts — including a fresh preamble for a queue-drained turn.
`InjectMessage` stamps the mailbox's **current** generation id onto the
`pendingInject` record at submit time, atomically with the append:

```go
func (mb *mailbox) inject(msg message.Message) {
    mb.mu.Lock()
    defer mb.mu.Unlock()
    mb.injects = append(mb.injects, pendingInject{
        msg:        msg,
        afterGenID: mb.current.id, // 0 (no owner) is a valid, meaningful value
    })
}
```

`beginGeneration` (called by `Run`'s loop before each turn, replacing
today's `activeRequests.Set(call.SessionID, cancel)` re-arm) does the
generation bump AND snapshots a `readWatermark` that `PrepareStep` will
compare against:

```go
func (mb *mailbox) beginGeneration(cancel context.CancelFunc) (genID uint64) {
    mb.mu.Lock()
    defer mb.mu.Unlock()
    mb.current.id++
    mb.current.cancel = cancel
    return mb.current.id
}
```

`PrepareStep`'s drain (replacing today's unconditional `injectQueue.TakeAll`)
becomes:

```go
func (mb *mailbox) drainInjects(genID uint64) []pendingInject {
    mb.mu.Lock()
    defer mb.mu.Unlock()
    var due, later []pendingInject
    for _, inj := range mb.injects {
        if inj.afterGenID <= genID {
            due = append(due, inj)
        } else {
            later = append(later, inj) // submitted against a FUTURE generation — not possible today, kept for forward-compat, see §6
        }
    }
    mb.injects = later
    return due
}
```

This alone does not yet solve duplication against the DB-history read,
because the race is specifically: DB write (step 1) happens, and the
in-flight turn's OWN `PrepareStep`/history reload may or may not have
already run past that row. The mailbox cannot know, from inside
`InjectMessage`, whether the owner's DB read already observed the new row
— that fact lives in the owner's own call to `a.getSessionMessages`, not
in the mailbox.

**The actual dedup key is the message ID, not a timing guess.** `runTurn`
already knows, at the moment it drains `injects`, the exact set of message
IDs it loaded into `history` for THIS generation (from
`a.getSessionMessages` in the preamble). The drain therefore filters by ID,
not by timestamp:

```go
due := mb.drainInjects(genID)
for _, inj := range due {
    if idAlreadyInHistory(history, inj.msg.ID) {
        continue // owner's own preamble read already picked this row up from DB
    }
    prepared.Messages = append(prepared.Messages, inj.msg.ToAIMessage()...)
}
```

This is a strictly stronger guarantee than "which generation id" alone
would give (generation id only bounds WHEN the inject was submitted
relative to `beginGeneration`, not whether THIS PARTICULAR turn's DB read
happened to include it — DB reads and generation starts are not
synchronized with each other today, and this design does not propose
making them so, since that would mean taking the mailbox lock across a DB
call). The combination — generation-stamped for the "which turn is
responsible" ownership question, ID-checked-against-loaded-history for the
"was it already in what I read" duplication question — closes both failure
modes in P1-1's description: duplication (ID check catches it) and loss at
a turn boundary (the mailbox `injects` slice is only ever cleared by
`drainInjects` itself, so an entry submitted after the owner's last
`PrepareStep` but before `drainOrRelease`/release simply survives in
`mb.injects` with a *future* owner's generation id greater than the id it
was stamped with — released untouched, so the very next `Run`/turn's
`drainInjects(newGenID)` still finds and consumes it exactly once, and its
own preamble DB read will already include it too, so the ID check
correctly treats it as "already in history" and it goes into `prepared`
zero times from the queue path but appears once via history — still
"consumed exactly once", just via the DB-read path instead of the
queue-splice path, which is the same outcome `InjectMessage`'s doc comment
already promises for the non-busy case today).

The cross-process inject path (`DrainPendingInjects`/`pending_injects`
table, `ConsumeInterruptInject`) is unchanged by this design — it already
does its own delete-after-read transaction at the DB layer and is
orthogonal to the in-process mailbox (see §8).

---

## 6. Summarization's future hook (not implemented now, per #268)

This task explicitly must not implement #268. The mailbox is designed so
that when #268 is picked up, compaction becomes another generation kind
instead of a sixth independent structure:

- `mailbox.compact *fantasy.ProviderOptions` (already in §1's struct) is
  the durable "a compact was requested while owned" record — the direct
  replacement for today's `summarizeQueue *csync.Map[string,
  fantasy.ProviderOptions]`.
- A compact becomes a **generation like any turn**: `beginGeneration`
  is called for it too, it gets its own `current.id`/`current.cancel`, and
  `Cancel`/`interruptAndReplace` targeting the session naturally reach it
  through the SAME `mb.current.cancel` field instead of a separate
  `sessionID+"-summarize"` lookup in `activeRequests`. This is what
  directly fixes P0-4's "concurrent summaries share one cancel-fn under a
  string key" defect and "manual summary can start while a Run is live"
  defect — both become impossible once starting ANY generation (turn or
  compact) is required to go through `submit`/`beginGeneration`, which
  already serializes on `mb.state`.
- The open design question #268 will still have to answer (and which this
  document deliberately leaves open, since it requires touching
  `runSummarizeCore`'s snapshot-then-delete logic): whether a compact
  generation and a turn generation are mutually exclusive (mailbox can
  only ever have ONE `current` at a time — simplest, matches "don't allow
  two compactions or a compaction+turn to overlap" from the review) or
  whether compact needs a second, orthogonal slot. This design assumes the
  former (one `current` generation per session, full stop) because nothing
  in the review's P0-4 section asks for concurrent compact+turn — it asks
  for the OPPOSITE, mutual exclusion — so `mailbox.current` being singular
  is not a limitation to route around later, it's the fix.

No mailbox method for `compact` is specified beyond the field declaration;
implementing `compact`/`decompactAfterRelease`/etc. is explicitly
out-of-scope and deferred to #268's own design pass.

---

## 7. Migration plan: incremental wrapper, then cutover per call site

**Recommendation: incremental, in three stages**, because the mailbox's
external contract (`submit`/`drainOrRelease`/`interruptAndReplace`/
`inject`) is expressible as a drop-in replacement for each existing
call site one at a time, AS LONG AS stage 1 introduces the mailbox
alongside the old structures without deleting them, and stage 2 migrates
callers one-by-one behind the SAME exported method names
(`QueueMessage`/`Cancel`/`InjectMessage`/etc.), so `sessionAgent`'s public
surface (§8) never has an intermediate broken state visible to
`coordinator.go`.

**Stage 1 — introduce `mailbox`, wire it up passively.**
Add the `mailbox` type and the `csync.Map[string, *mailbox]` field to
`sessionAgent`. Do not remove `activeRequests`/`messageQueue`/
`injectQueue`/`sessionStartMu` yet. No behavior changes; this stage is
pure addition and is safe to land and test in isolation (unit tests for
`mailbox` alone, no `sessionAgent` wiring).

**Stage 2 — migrate `sessionAgent` methods one at a time, each is its own
PR/commit:**

1. `tryReserveSession`/`releaseSessionReservation` → `mailbox.submit`/
   `mailbox.drainOrRelease`. This alone fixes P0-3 (§3) and is the
   highest-value, self-contained first cut — it only touches `Run`'s
   entry and the two `PopFront` call sites (end-of-turn and
   `runSummarizeCore`'s own drain), not `Cancel`'s clearing bug.
2. `QueueMessage`+`Cancel` → `mailbox.interruptAndReplace` for the
   `InterruptAndSend`/`requeueInterruptMessage` call sites specifically
   (coordinator.go:2024-2025, 2053-2054), while leaving `Cancel`'s
   OTHER caller (a bare abort with no replacement — Ctrl-C, `sessions
   kill`, cost/token-cap abort inside `OnStepFinish`) on a simplified
   `Cancel` that now ONLY cancels `mb.current.cancel` and does not touch
   `submitted`/`injects`/`replacement` at all (§4's last paragraph).
   This fixes P0-2.
3. Split `runCtx` into `dispatcherCancel` (`Run`'s own outer context) +
   per-generation `current.cancel` (already required by step 2's
   `interruptAndReplace`, since it needs a generation-scoped cancel to
   return) and route `runTurn`'s cancelled-preamble path through
   `drainAfterCancel` instead of returning `hasNext=false`
   unconditionally. This fixes P1-2 and MUST land together with or after
   step 2 (interruptAndReplace's generation-cancel target is meaningless
   without the split).
4. `injectQueue` → `mailbox.inject`/`drainInjects`, plus the
   history-ID dedup check in `PrepareStep`. Independent of steps 1-3;
   can land before or after them. Fixes P1-1.
5. Delete `activeRequests`/`messageQueue`/`injectQueue`/`sessionStartMu`
   and the `sessionID+"-summarize"` key usage (leaving `mb.compact` as an
   unused-for-now field per §6) once every call site has moved to the
   mailbox equivalent. This is the only step that requires ALL of 1-4 to
   be done first — it's a deletion, not a behavior change, so it is low
   risk once nothing reads the old fields anymore.

**Why incremental is safe here (and a big-bang cutover is not needed):**
every old structure maps to exactly one mailbox field/method (§2's table
is a 1:1 mapping, not a many-to-one collapse that would force simultaneous
replacement), and `sessionAgent`'s method signatures — the only thing
`coordinator.go` and the web server depend on — do not change at any
stage. The one place incrementalism has a hard ordering constraint is
step 3 depending on step 2 (a generation-scoped cancel is useless until
`interruptAndReplace` exists to hand it out) — noted above, not a blocker
to the overall incremental strategy, just a two-step atomic unit within
it.

**Testing per stage:** each stage should add a deterministic seam test
mirroring the release gate's regression list (`docs/reviews/2026-08-04-
multi-agent-stability-follow-up.md`'s "Минимальный release gate" section,
items 2-4 map directly to stages 2/3/4 above) BEFORE moving to the next
stage — do not batch verification until the end.

---

## 8. Explicitly unchanged

- **Kill path**: `session.KillProcess` (`internal/session/kill_windows.go`,
  `kill_unix.go`), `SessionLock.Release`/`RecordActivity`
  (`internal/session/lock.go`), and every `sessions kill`/`sessions reset`
  code path in `internal/cmd/sessions_kill.go`/`sessions.go`. The mailbox
  is purely in-process/in-memory; the OS-level lock and cross-process kill
  mechanics (P0-5, a separate task) are untouched by this design. A killed
  process's mailbox simply vanishes with the process — no persistence
  requirement is introduced.
- **MCP tool events**: nothing in `internal/agent/tools/*` or the MCP
  client/server plumbing reads or writes `activeRequests`/`messageQueue`/
  `injectQueue` today, and the mailbox does not change that boundary. Tool
  execution continues to run inside a generation's `genCtx` exactly as
  today (derived from `mb.current.cancel`'s context instead of directly
  from `runCtx`, which is an implementation-internal rename, not a new
  dependency).
- **DB schema**: `pending_injects`, `sessions`, `messages` tables and their
  Go-side `session.Service`/`message.Service` methods
  (`DrainPendingInjects`, `ConsumeInterruptInject`, `CreateAgentToolSessionID`,
  etc.) are unchanged. The cross-process inject/interrupt path
  (`crush sessions inject [--interrupt]`) continues to write through the DB
  exactly as today; the mailbox only affects the SAME-PROCESS in-memory
  hand-off, which is what P0-2/P0-3/P1-1/P1-2 are actually about. Cross-
  process pending-inject rows are drained into `prepared.Messages` by
  `PrepareStep` exactly as before (still a separate code path from
  `mailbox.injects`, see §5's last paragraph) — this design does not fold
  the two together, since the DB-backed cross-process path already has its
  own correct delete-after-read transaction and doesn't share the same-
  process race the mailbox exists to fix.
- **External CLI/API surface**: `SessionAgent` and `Coordinator` interface
  method signatures — `Run`, `Cancel`, `CancelAll`, `IsSessionBusy`,
  `IsBusy`, `QueuedPrompts`, `QueuedPromptsList`, `ClearQueue`,
  `QueueMessage`, `InjectMessage`, `Summarize`, `SummarizeQueued`,
  `TakeSummarizeQueue`, `CancelQueuedSummarize`, `SetTimeoutOptions`,
  `Model`, and on the coordinator side `RunWithOverrides`,
  `InterruptAndSend`, `InjectMessage` — keep their existing parameter and
  return types verbatim (`internal/agent/agent.go:271-316`,
  `internal/agent/coordinator.go:226-277`). Every caller in
  `internal/server`, `internal/cmd/sessions_*.go`, and the web handlers
  continues to compile and behave the same from the outside; only the
  private implementation behind those methods changes. `SessionAgentCall`'s
  fields (`internal/agent/agent.go:241-269`) are unchanged — no new field
  is required on the public struct; `pendingInject.afterGenID` and
  `generation.id` are mailbox-internal bookkeeping never exposed to
  callers.

---

## Open questions for the orchestrator/user

1. **`submitted` as a single-slot vs. true FIFO.** Today's `messageQueue`
   is an unbounded per-session FIFO (`csync.KeyedQueue`), even though in
   practice `Run`'s loop drains one call and returns it to the next
   iteration before checking again, so depth rarely exceeds 1-2 in
   production. This design keeps `mailbox.submitted` as a `[]SessionAgentCall`
   (unbounded, same as today) rather than collapsing it to a single slot —
   but I have not verified there is no caller today that relies on
   `QueuedPromptsList` returning more than one item in normal (non-buggy)
   operation. If there IS such a caller/UI affordance ("N messages queued"
   display), the FIFO must stay a real FIFO; if not, a single-slot mailbox
   would be simpler. **Needs a decision before stage 2, step 1's PR is
   written**, since it affects `drainOrRelease`'s return type
   (`SessionAgentCall` vs `[]SessionAgentCall`).
2. **Generation id scope and overflow.** `generation.id` is proposed as a
   per-session `uint64` counter, reset never (lives as long as the process,
   like every other in-memory session-agent field today). This is
   effectively unbounded in practice (2^64 turns) so overflow is not a
   real concern, but I want to flag that if the mailbox is ever persisted
   or shared across process restarts (not proposed here, but a natural
   next ask given the fork's multi-process session model), the generation
   counter would need to become durable too — out of scope for this task,
   flagging so it isn't assumed solved.
3. **Where `beginGeneration` is called for the summarize-inline case.**
   `runTurn`'s `shouldSummarize` branch currently calls
   `runSummarizeCore` INLINE (same goroutine, same stack, still holding
   the outer reservation) rather than as a separate generation — see §6's
   note that #268 has to decide whether compact is a fully separate
   generation kind or stays inline-within-a-turn-generation. This
   document's §6 assumes compact becomes its own generation for the
   STANDALONE `/compact` path (`runSummarize`, no `Run()` on the stack)
   but says nothing about whether the INLINE auto-summarize-mid-turn path
   (`shouldSummarize` in `runTurn`) should also start a new generation or
   stay inside the calling turn's generation. I did not resolve this
   because #268 is explicitly out of scope, but the orchestrator should
   know it is a genuinely open modeling question, not an oversight.
4. **`compact` field's interaction with `interruptAndReplace`.** If a
   compact is pending (`mb.compact != nil`, once #268 implements it) and
   an interrupt-and-replace arrives, should the interrupt cancel the
   compact generation the same way it cancels a turn generation? This
   design's `mb.current` being singular (§6) implies yes — but that means
   an in-flight compact could be interrupted mid-stream by an unrelated
   "interrupt and send" click, which has different data-loss implications
   (a half-written summary message vs. a half-written assistant turn).
   Flagging for #268, not resolved here.
