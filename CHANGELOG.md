# Changelog

User/operator-facing changes to this fork. Not to be confused with
`CHANGELOG.fork.md`, which tracks upstream-merge decisions for future
mergers — this file tracks what actually changed in behavior.

Format loosely follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added

- **`crush models use --large`/`--small`** — the two positional args
  (`crush models use <large> <small>`) always set large and small
  together, so there was no way to change just the fast/small model (or
  just the smart/large one) without retyping the other. `--large` and
  `--small` now exist alongside the already-independent `--worker`/
  `--reviewer` flags, so any of the four slots can be set on its own —
  e.g. `crush models use --small glm4_7_flash`. The positional form and
  the `--large`/`--small` flags are mutually exclusive per call (mixing
  them is rejected with a clear error, rather than silently preferring
  one).

### Fixed

- **Nine release-blocking concurrency bugs from the 2026-08-07
  release-concurrency review, closed and independently verified** (see
  `docs/reviews/2026-08-07-release-concurrency-review.md`). The review's
  central invariant: once an action is accepted, exactly one of three
  things must be true — a live owner is responsible for it, a freshly
  started runner is responsible for it, or a durable record exists that
  a future worker can claim. Every owner-exit path (success, cancel,
  error, panic, shutdown, or OS-lock contention) must transfer or
  reject all pending work atomically; nothing may be silently dropped.

  - **Ownership exit could silently drop an accepted call** — both the
    normal end-of-turn path and manual compaction's exit path had
    windows where a call already accepted into the mailbox could be
    lost instead of handed off or requeued. Replaced with one unified
    ownership-exit finalizer used by both paths.
  - **A call orphaned by OS-lock contention during a detached restart
    could be lost after retries were exhausted** — the retry loop gave
    up silently on persistent contention. It now requeues the call for
    a future `Run()` durably, and the same fix applies to the
    cross-process interrupt-inject path (the `pending_injects` row is
    recreated instead of vanishing after `ConsumeInterruptInject`
    already deleted it).
  - **Web "rerun message" could race a still-stopping session's
    transcript writes** — `rerun` now fails closed (rejects the
    request) instead of proceeding while the session's prior turn is
    still tearing down.
  - **Manual/standalone compaction could summarize using a model or
    provider that had already changed underneath it, and a failed
    cleanup write could be silently skipped if the context was
    already cancelled** — the summary snapshot (model + provider) is
    now captured immutably at the start of the summarize call, and the
    final DB cleanup write uses a cancel-immune bounded context so it
    still runs even after the parent context is gone.
  - **Shutdown could return before live dispatchers actually stopped**
    — `App.Shutdown()` fired a 5-second context that nothing
    unconditionally waited on. It now genuinely joins the live
    dispatchers (bounded, so a stuck one can't hang shutdown forever)
    and logs a warning if it had to force-exit while an agent was
    still busy.
  - **Web "busy"/"queued" events and the summarize-queue drain didn't
    track real mailbox ownership** — a client could see `Busy: false`
    broadcast while another owner still held the session, and a
    summarize request queued from a non-web path (CLI, detached run)
    could sit un-drained. Both are now tied to the mailbox's actual
    ownership transitions instead of a best-effort approximation.
  - **Manual compaction released the OS session lock through a raw,
    synchronous call instead of the same `mbReleasing` state machine a
    normal turn uses** — same-process observers (`IsSessionBusy`,
    `Cancel`) could briefly see "idle" while a slow lock release was
    still in flight. Manual compaction now goes through the same
    `beginRelease`/`finishRelease` epoch-protected transition as a
    normal turn's drain.
  - **Manual/standalone compaction had no idle-timeout watchdog, and
    provider cancellation wasn't a tested execution-boundary
    contract** — an HTTP provider stream could ignore a cancelled
    context indefinitely with nothing catching it. Compaction now runs
    under the same stream-watchdog mechanism as a normal turn, and
    provider cancellation is now covered by a conformance test.
  - **`internal/session` lock release cleaned up best-effort diagnostic
    metadata before the actual OS-level unlock**, so a hang in the
    diagnostic part could delay the real unlock. `SessionLock.Release()`
    now unlocks first, unconditionally, before touching diagnostics.
  - **Final audit pass**: two of the nine regression tests added by the
    fixes above proved only that a call was durably *stored* (in a
    queue or a recreated DB row), not that it was ever actually
    *executed* by a subsequent owner — the review's own "persistence is
    not execution" bar. Both were strengthened with a second phase that
    releases the OS lock, triggers the real pickup path, and asserts on
    an atomic provider-call counter plus message history.

- **Thirteen more release-blocking concurrency findings from the
  2026-08-09 follow-up review round** (see
  `docs/reviews/2026-08-09-release-concurrency-followup-review.md` and
  `docs/reviews/2026-08-09-oh-round-review.md`), closed and
  independently verified with an executable release-gate suite (see
  `docs/release_gate_summary.md`, `docs/release_gate_report.md`,
  `go test -run TestReleaseGate ./internal/agent/... ./internal/app/...`):
  - **`SessionLock.Release()` could still leave the mailbox permanently
    stuck in `mbOwned`/`mbReleasing`** if the unbounded filesystem I/O
    in its diagnostic-metadata cleanup hung — the OS-level unlock and
    file close now happen synchronously, first, unconditionally; the
    best-effort diagnostic-metadata cleanup (clearing the stale PID a
    `sessions kill`/`sessions why` reader could otherwise see) moved to
    a goroutine that Release() waits on for a short bound (50ms) before
    giving up and letting it finish in the background — long enough to
    cover real disk I/O, short enough that a genuinely stuck filesystem
    still can't meaningfully delay the caller. (A stricter guard that
    re-checked the OS lock before touching the metadata was tried and
    reverted: under real scheduler contention it occasionally collided
    with the SAME process's own fast re-acquire of a session it had just
    released, which was a worse regression than the narrow, accepted,
    cosmetic risk it was meant to close — see `sessions_kill.go`'s
    re-probe of the real OS lock immediately before any destructive
    action, which is what actually guards against a stale PID mattering.)
  - **Detached/orphaned calls that exhausted their retry budget had no
    guaranteed runner** — some could be lost entirely instead of
    landing in a durable queue. Replaced the three separate ad hoc
    detached-run policies (`restartOrphaned` / `restartOrphanedWithRetry`
    / `startDetachedRun`) with one durable per-session run queue
    (`session_run_queue`) and an independent `RunQueuePump` that
    autonomously picks up and executes queued calls, including across
    process restarts.
  - **Retry tails could requeue and re-execute a call that had already
    left a persistent trace**, producing duplicate provider calls and
    duplicate messages — retryable vs. terminal failures are now
    classified via an `AlreadyAttempted` marker interface
    (`ErrCallAlreadyAttempted`), and a call already recorded is never
    replayed.
  - **Manual/queued summary (`/compact`) could pick up a different
    session's model or provider mid-flight** if models were changed
    concurrently — it now resolves and freezes one immutable snapshot
    (model, provider options, prompt prefix) from the target session at
    the start of the call.
  - **A second manual `/compact` requested while the first was still
    running could get stranded** — the queued request is now drained
    and coalesced from every terminal ownership transition, not just
    the web handler's own code path.
  - **Shutdown could close the database while a non-cooperative agent
    was still genuinely running**, and forced shutdown could hold the
    global DB connection-pool mutex across a slow `sql.DB.Close()` —
    graceful shutdown now does a real join on every live dispatcher
    (not a polling loop) before releasing the DB, and forced shutdown
    skips the DB release entirely rather than risk closing it under a
    live writer.
  - **Provider cancellation on ctx timeout wasn't a verified contract
    per adapter** — added a conformance suite covering every HTTP
    provider category (openaicompat, openai, anthropic, azure, bedrock,
    vercel, openrouter) plus the CLI-provider process-tree-kill path,
    each proving a hung stream stops within 5 seconds of cancellation.
  - **A durable-queue entry that exhausted its max retry attempts could
    never actually be removed from the queue** — the cleanup call
    required the entry to be in the `leased` state, but an
    attempts-exhausted entry is scanned while still `pending`, so the
    delete silently never matched and the pump re-attempted (and
    re-failed) the same cleanup on every tick, forever. Fixed to lease
    the entry before the terminal-fail delete.
  - Several package-global mutable test seams used under `t.Parallel()`
    (a data-race and cross-test-pollution risk) were replaced with
    per-instance functional options, and a batch of regression tests
    from this round that had silently weakened over revisions (sleep-
    based races, gutted fault injections, mock-only coverage of the
    riskiest changes) were rewritten to restore their original
    guarantees.

- **`sessions locks` showed a healthy, actively-working session as
  "offline"** — observed live: a session sat at `PULSE_AGE == ELAPSED ==
  36s` while the process was alive and running real tool calls. The
  heartbeat mtime only advances on LLM-stream-chunk-gated activity, not
  on tool-call execution itself (a deliberate earlier design choice, to
  avoid a hung tool being masked as healthy by a timer that fires on
  tool-open alone) — so a session can legitimately run several tool
  calls in a row with no heartbeat touch for well over the offline
  threshold. The underlying call-tree activity signal already tracked a
  session's own top-level activity correctly; the display code was
  discarding it whenever it didn't come from a sub-agent delegation.
  Fixed to use the freshest signal regardless of source, while still
  only labeling it as a sub-agent delegation when it actually is one.

- **Two messages created in the same second could be permanently
  skipped in `sessions watch`/`sessions tail`, not just printed out of
  order** — timestamps are stored with one-second precision, and
  same-second ties used to fall back to comparing message IDs (random
  UUIDs). Whenever that comparison happened to lose, the message was
  silently dropped for good: the "already printed" cursor never moved
  past it, so the same losing comparison repeated identically on every
  later poll. The database already orders messages deterministically
  (insertion order as a tiebreaker); the display code now trusts that
  order instead of re-deriving a worse one.

- **`sessions pick` could hand off to a session in the wrong data
  directory** — after picking a session with `--data-dir` pointed
  somewhere non-default, the follow-up `sessions tail`/`sessions last`
  it spawns didn't forward that flag, so the child re-resolved to the
  default location and reported "session not found" for the very
  session just displayed.

- **Same-named file attachments uploaded within the same second could
  silently overwrite each other** — attachment filenames were built
  from a one-second-precision timestamp plus the original filename
  alone; two uploads of e.g. `report.txt` in the same second landed on
  the same path, and the second silently replaced the first's content.

- **Concurrent goroutine diagnostic dumps from one process could
  collide** — several stuck sub-agent turns firing the stream watchdog
  within the same second could overwrite or interleave each other's
  diagnostic dump, exactly when that evidence is most needed.

- **The hard-cap watchdog's diagnostic log could report a near-zero
  turn duration for a multi-hour timeout** — a copy/paste-style gap
  meant the out-of-tool branch reported time since the last stream
  chunk instead of the actual turn length, which is misleading
  precisely in the one situation (a postmortem) this log line exists
  for. Does not affect the timeout decision itself, only its own log
  message.

- **Shutdown could still begin a brand-new turn, and an interrupt during
  shutdown was reported as accepted** — a closing review of the fixes
  below found the shutdown work only half done. Refusing to hand more
  queued work to a turn already in progress does not stop a *new* one
  from starting: sessions are tracked lazily, so a request for a session
  the shutdown sweep never saw got fresh state and ran a full turn that
  nothing would then cancel. Shutdown now refuses new work at the agent
  level, before any session is claimed, and says so with a clear error
  instead of silently accepting a request that could never run. An
  interrupt arriving during shutdown likewise no longer reports success
  for a message that will not be executed.

- **A hung title could overwrite a good one, repeatedly** — a regression
  from the title fix below. Once a stuck title attempt is abandoned, a
  later turn generates a title normally; the abandoned attempt could then
  finish and overwrite it with the placeholder name, which in turn made
  the next turn try again, and so on. A late-finishing attempt now only
  fills the title in if it is still empty.

- **A delegated sub-agent could hang forever on its very first command,
  making the parent look busy while nothing happened at all** — the most
  visible bug of this round, reproduced live: a session sat in
  `delegating` with a healthy heartbeat for 41 minutes while its
  sub-agent was stuck on a bare `wc -l`. A non-interactive `crush run`
  auto-approves only the session id it was given, but a sub-agent runs
  under its own child session id, so its first command outside the
  read-only safe list waited on a permission prompt that does not exist
  in that mode. Sub-agents now inherit their parent's auto-approve
  status. Deliberately inheritance and not a blanket grant: a sub-agent
  of an interactive session still prompts as before, and
  `--restrict-run` still applies on top.

- **Shutdown could start a new turn instead of stopping** — cancelling
  every session on exit cancelled only each one's current turn, and the
  cancel-handling path then immediately pulled the next queued message
  in and began a fresh provider request, on a context nothing had
  cancelled. The process then tore its database down underneath an agent
  that was still working. Shutdown now latches every session closed
  first, so a cancelled turn can no longer pick up more work, and
  cancels the whole run rather than just the current turn. Anything
  still queued is left in place — it is already saved, and a later run
  picks it up — rather than being discarded.

- **An interrupt sent just as a turn finished could be accepted and then
  never run** — the interrupt was recorded and reported as successful,
  but only the cancellation path ever looked for it. When the cancel
  lost the race to the turn's own completion (which it can), the normal
  end-of-turn path released the session without ever seeing the pending
  message. Every end-of-turn decision now checks it.

- **Interrupting an idle session accepted the message but never ran it**
  — with no turn in progress there was nothing to interrupt, and the
  message was queued for a runner that did not exist, while the UI was
  told it had been queued successfully. It now starts a run.

- **A hung title generation could hold a whole turn open indefinitely** —
  the turn waited unconditionally for its background title goroutine,
  which is itself unbounded when a provider ignores cancellation. The
  session's ownership and lock stayed held for work that had already
  finished. The wait is now bounded; a title that overruns is abandoned
  and still saves itself if it eventually completes.

- **A wedged tool looked exactly like a working one** — the session
  heartbeat was refreshed on a timer for as long as any tool call was
  open, regardless of whether anything was happening, so `sessions
  locks`/`why`/`list` reported a stuck session as healthy for up to 45
  minutes (38 minutes of false "activity" were observed in practice).
  Activity is now recorded only for real progress. Long delegations stay
  covered without the timer: a sub-agent's genuine progress already
  propagates up through every ancestor session.

- **`sessions watch` spammed a once-per-second "sub-agent active" line
  and hid what the sub-agent was actually doing** — the line's throttle
  compared its own text, which embedded a live-updating age, so it
  changed every second and never throttled. Watch now live-tails
  delegated sessions the same way it shows the main session — real tool
  calls and results as they land, prefixed so they stay distinguishable
  — and covers every session in the call tree, so parallel delegations
  and a sub-agent's own sub-agent are visible too.

- **An interrupt or cancel landing right after a normal follow-up message
  was queued mid-turn could also silently do nothing** — a fourth
  independent review found the same defect the fix above closed on the
  legacy-queue reclaim path was still present on the far more commonly
  hit path: the current, non-legacy way a follow-up message gets queued
  while a turn is running. Fixed the same way — restore a working cancel
  handle at the moment ownership continues to the next turn — plus a
  small hardening (explicitly re-assert ownership state on reclaim
  instead of relying on it already being true) aimed at the same class
  of defect before it can resurface on a future code path.

- **A same-process concurrent send landing right as a turn finished could
  get a spurious "session already in use" error and be silently dropped,
  and an interrupt/cancel landing in the legacy-queue reclaim window could
  silently do nothing** — a third independent review of the mailbox
  migration (following the two fixes above) found no remaining bugs
  inside the mailbox's own state machine, but found two narrower defects
  at the seam between the mailbox and the OS-level session lock/cancel
  lifecycle. The end-of-turn drain used to flip the mailbox to "idle"
  well before the OS-level session lock was actually released (only once
  the whole call unwound, including up to several seconds waiting on
  title generation) — so a second call for the same session could
  legitimately see "not busy," try to start, and hit "already in use"
  from its own process's not-yet-finished prior turn, with the message
  lost rather than requeued. Fixed by releasing the OS lock and flipping
  the mailbox to idle as one atomic step, so "not busy" now always means
  the lock is genuinely free. Separately, reclaiming ownership from the
  legacy message queue left both of the mailbox's cancel handles pointing
  at nothing usable until the reclaimed turn got underway — a Cancel or
  interrupt landing in that narrow window silently did nothing (an
  interrupt-and-replace even reported success while cancelling nothing).
  Fixed by restoring a working cancel handle at the same moment ownership
  is reclaimed.

- **A message queued via the legacy path could still permanently wedge a
  session busy, and a message dropped on turn error was lost instead of
  retried** — two narrower follow-ups to the P0-2/P0-3 mailbox-migration
  fixes above, found by a second independent review of the fix itself.
  Reclaiming ownership from the not-yet-migrated legacy message queue
  used to grant a brand new ownership era without telling the turn loop,
  which kept using its original (now stale) era id for the rest of the
  call — the next release attempt then saw a mismatch, assumed a
  different owner held the session, and left it stuck busy forever with
  nobody running it. Reclaiming now explicitly continues the SAME era
  instead of starting a new one. Separately, a message still queued when
  a turn ended on a genuine error (not a cancellation) used to be logged
  and discarded by the cleanup step; it's now put back on the queue so
  the next run picks it up, matching how a stale reservation used to
  recover before the migration.

- **`crush run` hung for a full 5 seconds on exit after any turn ran, and a
  turn that errored out with a message still queued could wedge that
  session permanently busy** — introduced by the per-session owner/mailbox
  migration's first two stages (P0-3, P0-2) and found the same day by an
  independent review of that migration. Two related defects:
  - `IsBusy()`/`CancelAll()` still read the `activeRequests` map directly,
    but the migration stopped clearing its plain-session-id entry on
    release (only the mailbox's own state is cleared now) — so
    `IsBusy()` returned `true` forever after a session's first turn, and
    `App.Shutdown()` (reached by every `crush run` via
    `defer a.Shutdown()`) always burned its full 5-second busy-drain
    timeout instead of returning immediately once idle. Both now read the
    mailbox's state directly.
  - `Run`'s cleanup step ran unconditionally on every return path — not
    just the pre-loop bail-outs it was meant for — including after the
    turn loop's own drain had already released the session, or after an
    early-error return that left work still queued with nobody left to
    run it. On the "still queued" branch it left the session marked
    busy forever with no owner; on the "already released" branch, if a
    concurrent turn had since claimed the session, it could silently
    flip that live turn's ownership back to idle out from under it. The
    mailbox now tracks which ownership era each caller was granted, so a
    stale cleanup call is a safe no-op instead of clobbering a different,
    later owner, and it always fully releases the session (logging what
    was dropped) instead of leaving it stuck.

- **Starting any `crush` process could kill a long-running session in a
  DIFFERENT process (release blocker)** — the startup recovery sweep
  (`recoverInterruptedTurns`, which runs on every process start) walked
  **every** session in the data directory, not just this process's own,
  and stamped `FinishReasonError: "Process restarted"` on any assistant
  message that lacked a finish part and was older than 30 seconds. It
  performed no cross-process liveness check of any kind. Since a turn is
  legitimately unfinished for as long as it runs — a sub-agent
  delegation is bounded at 45 minutes — essentially every non-trivial
  turn was exposed: a sibling `crush` merely starting up marked it as
  crashed. Worse, `message.Update` rewrites the whole `Parts` blob from
  the snapshot the sweep read, so the stamp also **clobbered** whatever
  the live owner had streamed in between. This hit the fork's core use
  case (N concurrent `crush run` sessions sharing one data directory)
  routinely rather than rarely; an observed 38-minute delegation was
  killed this way and produced zero file edits. The sweep now proves no
  other live process owns a session — via `session.InspectSessionLock`,
  the same discipline `sessions kill`/`locks`/`reap` already use —
  before touching it, and skips it entirely if a live holder exists.
  The 30-second age threshold is demoted to a secondary check. A
  genuinely orphaned turn is still recovered exactly as before.

- **A message queued at the exact instant a turn released its session
  reservation could be silently orphaned (P0-3)** — the owning turn's
  final check ("is anything queued?") and the reservation release
  (`activeRequests.Del`, run from a `defer` back in `Run`) were two
  separate, non-atomically-linked steps. A concurrent send landing in
  the gap between them would see the session still marked busy, queue
  itself, and then nobody was left watching that queue: the owner had
  already decided "nothing queued" and returned, and its deferred
  release ran afterward with no further check. First step of the
  per-session owner/mailbox migration
  (`docs/plans/2026-08-04-session-owner-mailbox-design.md`) closes
  this: the "is anything queued" check and the reservation release now
  happen inside the SAME lock (`mailbox.drainOrRelease`), so a
  concurrent send can only ever land before or after that atomic
  transition — either the departing owner picks it up, or the sender
  becomes the new owner — never in a gap where neither does. Also
  applies to the standalone `/compact` follow-up drain that had the
  same shape.

- **"Interrupt and send" discarded the very message it was sending
  (P0-2)** — `InterruptAndSend` queued the replacement and then called
  `Cancel`, and `Cancel` unconditionally cleared the message queue — so
  the replacement was deleted by the line immediately after the one that
  queued it, on every call, deterministically. The web UI's interrupt
  button and `crush sessions inject --interrupt` therefore silently
  dropped the user's new request. Both now route through one atomic
  `InterruptAndReplace` operation that records the replacement and
  cancels only the in-flight generation under the same lock, so the
  turn loop stays alive to run it next. Related, previously unnoticed:
  a **bare** cancel (Ctrl-C, `sessions kill`, a cost/token cap) also
  wiped anything a caller had queued moments earlier for unrelated
  reasons; `Cancel` no longer touches queued work at all, leaving
  `ClearQueue` as the one explicit "discard everything" operation.

- **`queue run`'s spawned children now inherit the parent's explicit
  `--data-dir`** — `queue run` resolves its own data directory once
  (honoring an explicit `--data-dir` on the parent invocation, or a
  configured `data_directory`) to open the queue DB and acquire
  `queue.lock`, but `runQueueTask` never forwarded that resolved path
  to the `crush run --session ...` subprocess it spawns per task. Each
  spawned child independently re-resolved its own data directory
  starting from `--cwd`, which diverges from the parent's when
  `--data-dir` was passed explicitly — a queued task's child process
  could read/write its session and messages against a different DB
  than the one the queue claimed the task from. `runQueueTask` now
  takes the parent's resolved `dataDir` and passes it through as
  `--data-dir` on the child's argv.

- **Web UI attachments now honor the configured data directory** —
  `saveAttachmentToDisk` (in `internal/server/handlers.go`) always wrote
  uploaded attachments to `<cwd>/.crush/attachments/`, hardcoding both the
  working directory and the `.crush` segment instead of using the
  resolved `data_directory`/`--data-dir`. With a non-default data
  directory configured, attachments landed in a location the rest of the
  app doesn't read from. It now takes the already-resolved data
  directory (the same `externalOwnershipDataDir` helper that
  `annotateExternalOwnership` uses) and writes
  to `<dataDir>/attachments/`; a nil-config edge case defensively falls
  back to the old `<cwd>/.crush` default rather than hard-failing an
  otherwise best-effort save.

A separate review flagged the session heartbeat as reporting "alive"
for a fully deadlocked process (no real progress, mtime still fresh)
and a backlog of eight lower-priority follow-ups from the stability
review below. Closed together, one task/commit at a time:

- **Session heartbeat now reflects real activity, not a blind timer**
  — the lock-file heartbeat touched its mtime on every 10s tick
  unconditionally, so a wedged session with zero forward progress
  still looked alive to diagnostics forever. It's now gated on actual
  activity recorded since the previous tick (`SessionLock.RecordActivity`),
  and that activity signal is wired through the agent's normal turn
  loop (every stream callback) and propagated up through a
  delegation chain, so a parent session's heartbeat correctly stays
  alive purely from a sub-agent's real progress while the parent is
  blocked waiting on it — not just during the parent's own stream
  callbacks. A follow-up review caught a real gap in this fix: the
  activity signal only covered stream callbacks, not a tool actually
  *executing* — a healthy session blocked on one long tool call (up to
  45 minutes) recorded zero activity for its whole duration, which
  `sessions locks`' auto-delete (age > 60s) and `sessions watch`'s
  liveness check (age > 20s) both still read as "the process is dead."
  `sessions locks` could then delete a *live* session's lock file
  (unlinking a still-held OS lock lets a second process create a fresh
  one at the same path — two processes, one session id) and
  `sessions watch` could print a false "session finished" summary for
  a session still actively working. Fixed: the watchdog now records
  activity on every tick a tool is in flight and healthy, not just at
  start/finish; both commands additionally verify against the real OS
  lock / process liveness before trusting a stale-looking mtime. A
  further review pass found the same stale-mtime blind spot in two more
  places: the web UI's session-ownership indicator could flicker off
  during a long tool call (letting the composer re-enable and a send
  fail with "already in use," or the live tail stop following); and
  `sessions inject --json` could report a running session as
  `persisted-offline`. Both now fall back to a real process-liveness
  check when the heartbeat mtime looks stale, matching `sessions
  locks`/`sessions watch`'s existing fix. Separately, `sessions locks`
  itself still ignored a configured `--data-dir`/`data_directory` (the
  same class of bug already fixed for `sessions kill`/`reset --force`)
  and read a lock's PID in a way that missed the Windows PID-sidecar
  fallback — both fixed. A residual, narrow TOCTOU window in the
  auto-delete probe (proving a lock is dead, then removing it as a
  separate step) is now explicitly documented rather than silently
  assumed airtight.
- Added missing regression coverage for the queued-message-continues-
  after-summarize/compact behavior (both the mid-turn auto-compact
  path and the standalone `/compact` path), which had zero tests.
- Fixed a pre-existing, unrelated data race in a stream-watchdog test
  helper (an unsynchronized counter shared between the test goroutine
  and a fake HTTP handler goroutine).
- **A `--timeout-hard-cap` fire could be misreported as the wrong kind
  of timeout, in two steps.** First, when the hard cap fired while a
  tool happened to be in flight, the watchdog internally misclassified
  it as a tool-specific timeout (reporting `toolTimeout=true` even
  though the never-freeze tool-pause backstop never fired) — fixed by
  correcting that boolean so the hard-cap-with-tool-in-flight case
  agreed with the plain hard-cap-on-idle case. That fix alone wasn't
  enough: both cases then collapsed into the SAME "not a tool timeout"
  signal as a genuine provider idle-stall, so the user-facing finish
  message still had only two branches ("Tool timeout" / "Stream
  stalled") and every hard-cap fire fell into "Stream stalled" —
  falsely blaming the provider and citing `idleTimeout` instead of the
  hard cap that actually fired. The watchdog now carries a three-way
  cause (tool timeout / hard cap / idle stall) all the way through to
  the finish message, which gets its own "Turn timeout" title citing
  the actual configured `--timeout-hard-cap` duration and blames
  neither the provider nor a tool.
- **`sessions kill` and `sessions reset --force` ignored a configured
  data directory** — both hardcoded the lock/data path to `<cwd>/.crush`,
  so an operator using `--data-dir` or a project's `data_directory`
  config silently got "no lock file found" instead of the rescue these
  commands exist to perform. Both now resolve the actual configured
  data directory. A follow-up review found the fix still diverged from
  `reset --force` for a *relative* `--data-dir` combined with `--cwd`
  (resolved against the wrong base directory) and depended on a config
  load path that made a 45s network call and could write to the
  operator's real global config as a side effect of just killing a
  stuck lock — both fixed; `sessions kill` now resolves the data
  directory the same way `reset --force` does, without any network or
  config-persistence dependency.
- Three follow-ups to the stdin idle-timeout fix: partial stdin
  content returned after the producer goes idle now carries an
  explicit marker (visible to the model, not just a log line) noting
  it may be truncated; a narrow boundary race that could silently drop
  a chunk arriving at almost the same instant the idle timer fired is
  now guarded against; and a test-only mutable-package-global pattern
  was replaced with dependency injection. The truncation marker also
  wrongly claimed "the producer went idle" when the real cause was a
  read error — it now names the actual cause.
- Removed a tautological sessions-kill test that never called the
  function it claimed to test, and documented (without attempting to
  structurally close, out of proportion for a manual rescue CLI) a
  narrow residual PID-reuse race between proving a session is held and
  actually killing its holder.

An independent review of the batch below (task-276 investigation + the
multi-agent stability review) found four more issues in the fixes
themselves, closed in the same way:

- **The parent/child delegation cleanup grace didn't actually change
  the race it fixed** — the 90s grace was added to both the parent's
  wait AND the child sub-agent's own watchdog identically, so it
  canceled out of the "child fires first" inequality algebraically
  and the parent still always won. The grace now applies only to a
  top-level (delegating) session; a sub-agent, which can never itself
  be waiting on a further nested delegation, gets none — so its own
  stuck-tool detection fires strictly before the parent's.
- **`crush sessions reset --force` had the same stale-PID kill bug
  just fixed for `sessions kill`** — a sibling code path that read a
  lock file's recorded PID and killed it unconditionally, missed by
  the original fix because it lives in a different file. Now routes
  through the same probe-before-kill helper.
- **A web `/cancel` during a queued turn's DB preamble could silently
  no-op** — the turn loop's cancel function was only re-registered
  once, before the first turn; from the second queued turn onward, a
  cancel request during that turn's DB preamble found a spent,
  already-fired cancel func from the previous turn and did nothing.
  Now re-armed before every turn.
- A second, near-identical `t.Parallel()` + mutable-package-global
  pattern (title-generation's own timeout, not the DB-preamble one
  fixed earlier) was converted to the same per-agent injectable field.

Two more, explicit product decisions/fixes:

- **`--agents single` vs. a configured Worker** — confirmed as
  intentional: a configured Worker model always wins over an explicit
  `--agents single` (unchanged runtime behavior). What WAS wrong was
  the documentation: the `--agents` flag's `--help` text, an inline
  code comment, and the installed `/crush` slash-command guide all
  flatly claimed `--agents single` was an absolute guarantee against
  delegation — the literal opposite of the real behavior, and
  actively misleading for an orchestrating agent deciding how to
  invoke `crush run`. All three corrected, no behavior changed.
- **stdin named-pipe read could hang indefinitely again** — the
  previous fix (above) bounded only the wait for the *first* byte,
  then read to EOF with no further timeout at all; a producer that
  wrote one chunk and then went silent forever (no close, no more
  data) hung `crush run` indefinitely. Replaced with a real idle
  timeout applied to the whole read, chunk by chunk, that resets on
  every chunk received — `crush run` can now only ever block for
  "the grace window since the last byte was seen," never longer,
  regardless of how the pipe behaves afterward.

A second, independent review of the fixes above (this time targeting
the follow-up commits themselves, not the original batch) found two
more things:

- **The `agentic_fetch` tool's own nested delegation wasn't
  classified as a sub-agent** — it runs through the same delegation
  path as a real worker sub-agent, but its `SessionAgent` never set
  `IsSubAgent`, so it was invisible to the parent/child cleanup-grace
  fix above and quietly kept the old symmetric-cancel-out bug on that
  one path. One-line fix.
- **The cleanup-grace fix's own documentation overstated what it
  guarantees** — it only protects a child that wedges *early*
  (roughly within the grace window of being delegated to); a child
  that works productively for a while and only wedges deep into its
  turn still loses the race to the parent's watchdog, same as before
  that fix. Not a correctness issue (the parent still terminates
  correctly, and cost accounting is already cancel-immune, see
  above) — just a claim that needed correcting before someone trusted
  it. Documented as an explicit, tested, known limitation instead of
  a silently overstated guarantee.

Concurrency and reliability hardening, found via an independent
external code review of the hot-swap-binary work and verified/fixed
one at a time:

- **`deploy.go`** — the Windows rename-aside replace could leave the
  target binary missing if the second rename failed after the first
  succeeded. It now rolls back to the original binary on that failure,
  uses a unique per-deploy temp path instead of a shared one, and
  documents the accepted risk for multi-destination replacement.
- **Session updates** — renaming a session (and other narrow session
  updates) went through a whole-row `Save` that could silently
  overwrite a concurrent agent turn's token/summary/todos update.
  Replaced with column-scoped updates (`Rename`, `SetUsage`,
  `SetSummaryAndUsage`, `SetTodos`); the broad `Save` method was
  removed.
- **Transcript pagination** — reading a delegation transcript page by
  page could drop or duplicate messages, because `created_at` is
  second-granularity and a single agent turn can produce dozens of
  messages within one second, with no secondary sort key to break
  ties deterministically. Pagination now orders by `created_at DESC,
  rowid DESC` and caps how deep an offset-based page can go.
- **WebSocket replay buffer** — the reconnect replay buffer was capped
  by event count only, not by total size, and evicted by reslicing
  (O(n) copy per eviction once full). It's now a real fixed-capacity
  ring buffer with a 16 MiB total byte budget and a 1 MiB per-event
  cap.
- **Background job limit** — `MaxBackgroundJobs` was checked against
  the total number of tracked jobs, including ones that had already
  finished and were only being retained so their output could still be
  read. It's now checked against a separate active-job counter, so
  finished jobs no longer block new ones from starting.
- **Permission race** — a duplicate or concurrent response to the same
  permission request (e.g. a UI double-click) could block the second
  caller forever on an already-drained response channel. Grant/Deny
  now atomically claim the pending request before responding; a
  late/duplicate response is a no-op instead of a hang.
- **HTTP debug logging** — with debug logging enabled, the HTTP
  transport used to buffer an entire response body in memory before
  returning it to the caller, breaking streaming responses even when
  the configured log level wouldn't actually emit anything. Response
  bodies now stream through immediately; a bounded preview is
  captured on the side and logged once the body is fully read.
  Retries on 5xx are now limited to idempotent requests.
- **WSL vs. Git Bash on Windows** — resolving a shebang script's `bash`
  interpreter could pick the WSL launcher (`System32\bash.exe`) ahead
  of Git Bash, which then fails on the Windows-style script path it's
  given. The resolver now skips the WSL launcher in favor of Git Bash
  when both are present, and returns a clear error if only the WSL
  launcher is available.
- **grep fallback timeout** — the memory-bounded fallback path could
  keep reading past an expired context on a file containing one very
  large line with no newline, since cancellation was only checked
  between lines. It's now checked periodically within a single line
  read as well.
- **Session fork** — forking a session copied its history through
  several unguarded writes with most errors silently discarded; a
  failure partway through could leave (and report success for) a
  partially copied fork. Forking is now one transaction that rolls
  back entirely on any failure.
- **Config file races across processes** — two `crush run` processes
  writing the config file at the same time could silently lose one
  process's change (in-memory locking only protected against races
  within a single process). Config writes now also take a
  cross-process file lock, reusing the same locking mechanism already
  used for MCP config writes.

Two further, independent full-project reviews (#152-171) turned up
additional concurrency, security, and reliability issues, fixed and
verified one at a time in the same way as the batch above:

- **Sub-agent sessions weren't cross-process locked** (#163, critical)
  — a sub-agent runs under its own child session id, distinct from
  its parent's, so the parent's inter-process lock never covered it;
  two processes could write into the same sub-agent session
  concurrently. The lock is now taken for sub-agent sessions too.
- **Config reload held the publish lock during shell substitution**
  (#164) — resolving `${...}` values in config could block on an
  external command for up to 5 minutes while holding the same lock
  every config read/write needs; the resolve timeout is now 30s and
  the slow disk+shell-resolve phase runs without holding the publish
  lock at all. The same fix replaced environment-mutating shell-var
  push/pop with a non-destructive overlay that never touches the
  process's real environment.
- **WebSocket handler panics could crash the whole `crush web`
  process** (#154) — a panic in any per-connection message handler
  is now recovered and logged with a stack trace instead of taking
  down the server; concurrent handlers per connection are also capped.
- **`permissionService.Request` held its lock for the entire wait on
  a human response** (#156), serializing every other session's
  permission prompts behind whichever one the user hadn't answered
  yet. The lock is now released before waiting.
- **Config write lock could stall for up to 30s under load** (#153),
  an availability regression from an earlier change; write-lock
  acquisition under an already-held publish lock is now bounded to 2s
  with a stall warning instead of silently blocking full-length.
- **`projects.Register` wasn't atomic across processes** (#157) — a
  read-modify-write with no cross-process lock and a non-atomic file
  write could corrupt the projects file under concurrent `crush run`
  invocations. It now takes the same file lock as config writes and
  writes atomically.
- **WebSocket `CheckOrigin` always returned true** (#159) — a
  cross-site WebSocket hijacking (CSWSH) exposure with only the
  `SameSite` cookie attribute as a mitigation. Origin is now validated
  against the actual bound host/port; token comparisons across
  cookie/bearer/query/body now use a constant-time compare.
- **Read-heavy operations serialized behind the single SQLite
  connection** (#161) — every read and write shared one connection,
  so a slow read (deep transcript pagination, a large call-tree query)
  queued every other database operation, including message writes,
  behind it. Reads now use a separate read-only connection pool that
  runs concurrently with the writer under WAL.
- **Grant/Deny could produce contradictory granted+denied outcomes**
  (#167) — the outcome was published before the atomic claim of the
  request; both are now inverted so only the actual race winner acts.
- **Config snapshots could be mutated after publishing** (#168) —
  some production code paths mutated already-published config objects
  in place instead of going through the copy-on-write path, breaking
  the immutable-snapshot contract readers rely on.
- **Two concurrent deploys could mix binary versions** (#169) — the
  hot-swap deploy steps weren't cross-process locked; a second deploy
  starting mid-way through the first could interleave rename
  operations. Deploys now take a cross-process lock around the
  rename-aside steps. The same task added keyset-based pagination for
  live transcript reads, replacing a racy separate count+fetch.
- **Token accounting could go non-deterministic** (#165) — background
  title generation and the main turn both wrote token counts through
  the same additive/overwrite path, racing each other. Title
  generation's cost is now tracked separately from the main turn's
  token/cost snapshot.
- **`cliprovider` could pick the WSL `bash.exe` launcher** (#166) on
  Windows when it happened to sit ahead of Git Bash/MSYS on `PATH`,
  the same class of bug fixed for shell scripts earlier (#149) but
  missed in the CLI-provider binary resolver at the time.
- **HTTP debug logs could leak request/response body secrets** (#170)
  — enabling `--debug` alone used to be enough to log full LLM
  request/response bodies (system prompts, message history, tool
  output, sometimes API keys echoed in JSON fields). Body logging now
  requires a separate `CRUSH_LOG_HTTP_BODIES=1` opt-in on top of
  `--debug`, and known secret-shaped JSON fields are redacted even
  when it's on.
- **npm cache key was spoofable via size+mtime** (#170) — the
  platform-package launcher's binary cache key was derived from file
  size and mtime, so an in-place binary replacement that preserved
  both (e.g. a deploy tool that restores timestamps) would keep
  serving a stale cached build. The key is now a SHA-256 hash of the
  binary's actual content.
- A further batch of lower-severity nits and nondeterminism fixes
  (#160, #162): grep's fallback reader no longer returns a spurious
  partial-match error on a clean EOF, `ListUserMessagesBySession`
  queries gained a tie-breaker for deterministic ordering, deploy's
  temp-file close errors are now checked, lock-contention errors are
  now a typed `ErrLockContended` instead of a string match, and
  several small goroutine/singleflight/atomic races were closed.
- **Orchestrator-mode prompt regression fixed before it ever shipped**
  (#171) — an uncommitted edit to the coder prompt template had
  compressed its worker-delegation rule to defer zero-trust
  verification of delegated chunks to a single pass at the end instead
  of verifying each chunk as it lands, letting mistakes compound
  across chunks; fixed to verify per chunk again, and the "under 4
  lines of prose" rule now explicitly exempts diagnosis/security-review/
  handoff turns instead of relying on an implicit carve-out in a
  different rule.

A concurrent sub-agent hang investigation (task-276) and a follow-up
independent stability review (`docs/reviews/2026-08-01-multi-agent-stability-review.md`,
range `66c4d062..e9544a8f`) turned up a chain of related lifecycle,
watchdog, and process-safety issues, all fixed and verified one at a
time in the same way as the batches above:

- **`crush run` could hang forever on a named pipe with no data
  buffered yet** — `MaybePrependStdin` used to block indefinitely on
  `io.ReadAll(os.Stdin)` for an inherited/piped stdin fd that never
  closes. It's now bounded by a grace window for the first byte, with
  the leaked reader goroutine documented as an accepted single-
  goroutine cost, not a hang.
- **...and then that fix silently dropped data a slow-but-real
  producer had already sent** — the first version raced the WHOLE
  read against the grace window, so a producer that wrote real data
  but hadn't closed the pipe within it lost everything, with the log
  actively claiming "produced no data" even though data existed.
  `stdinReadGrace` now only bounds the wait for the *first* byte; once
  a producer proves it's alive, the rest is read to EOF with no
  further timeout.
- **A wedged sub-agent delegation could freeze the whole parent
  process with zero diagnostics** — an earlier fix that exempted
  sub-agent delegations from the stream watchdog's tool-execution cap
  removed the cap entirely instead of raising it, so a hung child
  could block the parent's turn forever with no error, no finish
  part, and no goroutine dump. The watchdog now applies one generous,
  always-finite cap (`toolExecutionMaxDefault`, 45m) to every tool,
  including delegations — never no cap at all — and captures a full
  goroutine dump to disk the moment it actually fires, so the next
  occurrence is diagnosable without a live debugger attach (which, on
  a stripped release binary, can only kill the process, not inspect
  it).
- **`crush sessions kill`/`ReadLockPID` couldn't identify a live
  Windows holder** — `LockFileEx`'s mandatory whole-file lock made a
  plain file read of the lock file fail for any genuinely live
  holder, so `sessions kill` on a live session used to see PID 0. A
  never-locked `.pid` sidecar file, written alongside the lock, is now
  the primary read path.
- **`crush -v` printed the same version string for every build** —
  `Commit`/`BuildTime` are now stamped into every build path
  (local dev build, goreleaser, the npm publish workflow) via
  `-ldflags`, so a deployed binary's provenance is identifiable.
- **`Run()`'s DB preamble had no timeout** — `sessions.Get`/
  `getSessionMessages`/`createUserMessage` ran on an unbounded context
  before the stream watchdog even starts, so a wedged single-writer
  SQLite connection (`SetMaxOpenConns(1)`) could hang a turn
  invisibly, with no watchdog running yet to catch it. Now bounded by
  a 60s timeout, injectable per-agent for tests instead of a shared
  package var.
- **`Run()` deadlocked on its own session lock while draining a
  queued message** — three call sites recursed into `a.Run(...)` from
  inside `Run`'s own still-executing stack frame, before the prior
  turn's `defer ipcLock.Release()` had run; since the inter-process
  lock isn't reentrant even within one process, the recursive call
  collided with its own parent and failed with "session already in
  use," silently dropping the queued message. Replaced with an
  explicit turn loop that reuses one lock acquisition across every
  queued turn. A related non-atomic busy-check/registration race
  (two concurrent `Run()` calls for the same session could both
  observe "not busy") was closed with an atomic check-and-claim.
- **A hung title-generation call could hold `Run()` open forever** —
  the background title goroutine ran on the raw, unbounded context
  instead of the per-turn context the stream watchdog actually
  cancels, so a stuck title provider blocked `Run()`'s return even
  after the main turn's own watchdog and the new DB-preamble timeout
  had both already fired correctly. Title generation is now derived
  from the turn's own cancellable context plus an independent 2-minute
  backstop.
- **A cancelled turn could permanently lose a sub-agent's spend** —
  the child-to-parent cost transfer ran on the same context as the
  whole turn, so a watchdog firing or a user Ctrl-C right as the
  child finished made the transfer's `BeginTx` fail immediately;
  the failure was only logged, and a one-shot sub-agent never invoked
  again meant that spend was gone from the parent's ledger for good.
  The transfer now runs on a short, cancel-immune detached timeout.
- **`sessions kill` could kill an unrelated process on a stale PID** —
  a cleanly-released lock left its old PID sitting in the lock
  file/sidecar; if the OS later recycled that PID for an unrelated
  process, `sessions kill` would force-kill it on trust alone. Release
  now clears the PID metadata while it still holds the lock, and
  `sessions kill` probes for a real OS-level lock before ever touching
  a PID — only a genuine contention error triggers the kill.
- **A parent delegation could be cancelled before its child had a
  chance to clean up** — after the tool-cap unification above, both
  the parent's wait and the child's own turn shared the identical
  cap, but the parent's clock starts counting earlier (from the
  moment it decides to delegate) than the child's own watchdog (which
  starts once the delegation is actually executing) — so the parent
  always won the race and force-cancelled the whole delegation before
  the child could persist its finish part, cost transfer, or
  diagnostics. The parent's wait now gets an additional 90s cleanup
  grace on top of the shared cap, so the child always gets a chance
  to unwind on its own terms first.
- **`--timeout-hard-cap` wasn't hard without
  `--timeout-extends-on-progress`** — the wall-clock hard-cap check
  outside of tool execution only ran inside the progress-extension
  branch; with that flag off (the default) and a provider that kept
  the stream alive with regular activity, an explicit hard cap was
  silently ignored. The check is now unconditional.

A third independent review pass over this same batch found five more,
lower-severity issues, closed the same way:

- **The watchdog's own goroutine-dump-on-fire could re-introduce the
  hang it was added to diagnose** — `onFire` wrote the diagnostic dump
  to disk synchronously before `cancel()` ran, so a hung/slow disk
  write could itself block cancellation indefinitely. The fire cause
  is now recorded first, then the (unawaited) dump write runs on its
  own goroutine, off the critical path to `cancel()`. A follow-up
  review found that fix went too far: dispatching the *entire* dump
  (including `runtime.Stack`'s goroutine-stack capture, not just the
  disk write) to an async goroutine meant the capture itself could
  race a fast unwind and record post-cancellation state instead of
  the actual hang, or never run at all if the process exited first.
  The stack capture (fast, no I/O, cannot block on disk) now runs
  synchronously in `onFire`, at the moment the hang is detected;
  only the write to disk is dispatched asynchronously.
- **Five more CLI commands ignored a configured data directory** —
  `sessions list`'s status column, `sessions reap`, `sessions watch`,
  `sessions why`, and `queue` all independently hardcoded or
  preferred a raw `--data-dir` flag over the resolved config, the
  same class of bug already fixed for `sessions kill`/`reset --force`/
  `sessions locks`. All five now use the same resolved data directory.
- **A failed lock cleanup in `sessions locks`' auto-delete path
  vanished silently** — when a lock was proven to belong to a dead
  holder but the subsequent file removal itself failed (e.g. a
  lingering open handle on Windows), the entry disappeared from the
  listing with no warning either way. It now surfaces a warning and
  falls through to the normal display path instead of silently
  dropping the entry. A follow-up review caught an over-correction in
  that same fix: it treated *every* removal failure as worth warning
  about, including the file already being gone (`fs.ErrNotExist`) —
  which happens routinely when a concurrent `sessions reap`/`kill`/
  `reset --force`, or another parallel `sessions locks` invocation,
  wins the race to delete the same stale lock first. That specific
  case is the removal's goal already being met by someone else, not a
  failure, so it's now reported as the normal success message (no
  warning, no phantom row for the vanished file) — the warning and
  display fallback are reserved for genuine removal errors.
- **A reused PID could pin a session's liveness indicator "alive"
  forever** — `InspectSessionLock`'s fallback to real process liveness
  when the heartbeat mtime looks stale (see above) had no upper bound
  on how stale the mtime could be before that fallback stopped being
  trusted; a sufficiently old lock whose PID got recycled by the OS
  for an unrelated process would read as live indefinitely. The
  fallback is now bounded to 60 minutes. A follow-up review pass found
  two more independent copies of the exact same unbounded check —
  `sessions watch`'s end-of-session detection and `sessions list`'s
  STATUS column — that hadn't received the bound; `sessions watch` now
  delegates to the same, now-bounded `InspectSessionLock` instead of
  re-implementing the check, and `sessions list` applies the same
  60-minute bound (exported as `session.MaxPidFallbackAge`)
  independently, since its "trust a confirmed-alive PID unconditionally"
  shape isn't a drop-in match for `InspectSessionLock`'s. A further review
  pass found a FOURTH independent copy in `sessions why`'s status explainer
  (the very command meant to diagnose this verdict) with the same unbounded
  trust; it now applies the same `session.MaxPidFallbackAge` bound too. All
  four known copies of this check are now bounded. A later pass found the
  fourth copy's fix left the printed *reason text* factually wrong in
  exactly the PID-reuse case it targets — it said the recorded PID "is not
  alive" when that PID was, in fact, genuinely alive (just untrusted due to
  lock age). `sessions why` now gives that case its own accurate wording
  ("no longer trustworthy — likely OS PID reuse") instead of reusing the
  genuinely-dead-PID phrasing. Two more review passes caught the reason
  text still wrong on both sides of that fix: the age-bound branch printed
  "likely OS PID reuse" unconditionally, even for the dominant case of a
  lock whose recorded PID is genuinely dead (now checks `IsProcessAlive`
  and only claims reuse when the PID is actually confirmed alive, and now
  also prints the lock's real age alongside the bound threshold), and the
  separate unreadable-PID case (`pid <= 0`, normal on Windows, but here
  combined with a stale heartbeat) claimed a fictional "holder PID 0 is
  not alive" — it now cites the real evidence, a stale heartbeat, instead
  of a PID that was never actually read.
- De-duplicated four independently-hardcoded copies of the
  "Stream stalled" finish-title string in internal/agent (the retry
  logic's own constant, the watchdog's actual production value, and
  two tests) into one source of truth, so a future reword of either
  side can no longer silently break transparent stall-retry matching
  without a test catching it. A fifth, cross-language copy in the web
  UI (web/src/components/Message.tsx — TypeScript can't import the Go
  constant) is intentionally not merged; it's kept in sync by a
  comment cross-reference on both sides rather than wiring the
  literal through the WS/JSON protocol (LOW severity).
- Two classes of this batch's own regression tests were themselves flaky
  under `-race` load; both are now deterministic. The PID-fallback
  boundary tests across `sessions_list_test.go`, `sessions_watch_test.go`,
  `sessions_why_test.go`, and `internal/session/lock_test.go` used a
  1-second timing margin around `MaxPidFallbackAge` that was too tight
  under load; widened to 2 minutes. A separate test in
  `internal/agent/stream_watchdog_test.go` — whose 100ms timing budget went
  stale once its diagnostic capture became synchronous by design — now
  uses a direct file-presence check instead of a timing budget that a
  widened timeout would have made unable to catch its own regression. Also
  fixed a read-before-write race in that same watchdog test (polled for a
  dump file's existence rather than its content, occasionally reading it
  mid-write) and made a probabilistic ENOENT-race regression test
  deterministic via a test-only hook instead of a goroutine race that could
  pass even against a reverted fix.

A further round closed the last P0-4 (summarization/compaction) gaps and
finished the mailbox migration:

- **Summarization could run outside session ownership and delete live
  history out from under a turn (P0-4)** — `/compact` and the auto-summarize
  path used a separate, non-atomic busy check from the mailbox's own
  turn-ownership tracking, so a manual `/compact` and an ordinary turn
  could both believe they owned a session at once; whichever finished last
  won, and the loser's in-flight message could vanish along with the
  history it referenced. Summarization now goes through the same atomic
  `beginCompact` check-and-reserve the mailbox uses for turns, so a
  compaction and a turn (or two compactions) are now mutually exclusive by
  construction, not by convention.
- **Cross-process compaction was not serialized — only in-process (P0-4
  residual)** — `beginCompact` closed the gap above only within one
  process; a *second* `crush` process holding the OS session lock could
  still start an ordinary turn while this process's compaction was mid
  commit, the same "one process deletes history the other believes is
  live" shape P0-4 itself was about. In a fork whose whole point is N
  concurrent `crush run` processes over one tree, that's a routine
  configuration, not a theoretical one. Manual `/compact` now takes the
  same OS session lock a turn takes, held across the compaction's commit
  phase, and releases it explicitly (not via `defer`) before handing off
  to a drained queued message's own turn — a `defer` would have kept the
  lock held across that hand-off and made the process reject itself with
  "already in use," naming its own PID. Inline compaction (already running
  under a turn's own lock) is unaffected; it never takes a second lock.
  Also hardened compaction's commit phase (writing the summary pointer,
  then deleting the old messages) against cancellation the same way an
  earlier fix protected the "silent" auto-compact path: a cancellation
  landing between the two writes used to leave a session's history in a
  half-updated, unrecoverable state. An independent review of this fix
  found two further issues, both closed the same day: manual `/compact`
  released its OS lock only AFTER flipping the session's in-memory state
  to idle, reopening the exact same-process "not busy, but the OS lock
  says otherwise" race `mbReleasing` exists to prevent — the lock is now
  released first; and manual `/compact` acquired its own OS lock but never
  wired it into the session's activity-notify chain, so a long-running
  manual compaction's lock file went heartbeat-stale for the same reason
  the next bullet describes for the mid-turn path — it's wired now, the
  same way.
- **Silent (background) compaction was invisible to the session
  heartbeat** — a healthy mid-turn compaction stream produced no heartbeat
  activity, so `sessions watch`/`locks` could report a working session as
  heartbeat-stale mid-compaction. It's wired the same way an ordinary
  turn's stream is. Separately, the same stream also now bumps the stream
  watchdog, so a compaction that legitimately runs longer than the idle-
  stall timeout is not killed as "no provider activity" with a misleading
  warning and a goroutine dump; this half only applies to compaction
  running under a turn's own watchdog (mid-turn/silent) — manual
  `/compact` has no watchdog of its own to bump, only the unrelated
  10-minute overall timeout it already had.
- **An interrupt landing between two turns of the same session could still
  kill the whole dispatcher instead of just redirecting it, and could
  drop the message that triggered the interrupt** — a follow-up to the
  DB-preamble interrupt fix above: the same race existed in the narrower
  window between one turn ending and the next one starting. Fixed the
  same way, plus a fix for the message itself: the pre-empted call that
  the interrupt was replacing used to be silently discarded rather than
  requeued, which meant that with two or more messages queued, exactly
  one was destroyed — chosen by scheduling luck, not deterministically.
  It's now put back at the front of the queue instead.
- **Finished migrating `QueueMessage` off the legacy message queue** — the
  fire-and-forget queue primitive was the last production caller of the
  pre-mailbox `messageQueue` structure, left behind when the rest of the
  mailbox migration landed. It now queues through the mailbox directly,
  and the legacy structure and its remaining call sites are gone. As part
  of this, `PrepareStep`'s behavior of folding an already-queued message
  into the current turn's own prompt was deliberately removed: in the
  mailbox model, each queued call is its own turn with its own DB record
  and its own response, and silently merging two into one broke that
  contract.
- **The release gate's own regression coverage had two real gaps, found
  by an independent audit that first gave a passing verdict its own
  evidence didn't support** — every gate scenario now has a test that is
  independently revert-checked (fix removed → test fails on its own
  assertion → fix restored), not just green. That process caught: a
  test asserting the wrong thing for one scenario had been accepted on
  the strength of an unexplained panic rather than a clean failure (now
  has a reasoned explanation tying the panic to the exact removed guard);
  and one scenario — an abandoned title-generation goroutine's deferred
  fallback overwriting a *later* turn's real title with the placeholder
  name — had no regression test at all despite being a named, previously
  fixed defect. It does now.
- **A final `@oh` review of the release gate suite above found six more
  real defects, then a second confirming pass over that very fix found
  six new ones introduced by it** — both rounds independently
  re-verified (never accepted from review narration alone), each with a
  genuine FAIL→PASS revert-check.
  - First pass: a durable queue entry whose lease mismatched the one
    the pump actually scanned could be terminal-failed by ID instead of
    released back to pending, deleting the wrong entry; a busy OS
    session lock (routine, expected contention from another live
    process) was counted as a failed attempt toward the queue's max-
    retries budget exactly like a genuine failure, exhausting healthy
    entries under load; `SessionLock.Release()`'s metadata cleanup ran
    fully async with zero wait, so a caller could observe a stale PID
    immediately after `Release()` returned; and a couple of doc-comment
    corrections.
  - Second pass, on the fix for the first: `App.New()` started the run
    queue pump *before* `InitCoderAgent` assigned the coordinator it
    reads — an unsynchronized data race per Go's memory model between
    `InitCoderAgent`'s write and the pump's read, plus a work-loss path
    where an already-started pump could dead-letter a durable entry if
    `InitCoderAgent` failed or the app was unconfigured. Fixed by moving
    pump construction and `Start()` to strictly after `InitCoderAgent`
    succeeds, removing the hazard architecturally. Also: a new pump-
    wiring regression test called real `app.New()` without environment
    isolation (the same failure class that once hung a stress run for
    9+ minutes, see `internal/cmd/providers_test.go`); removing the
    lease-attempt increment from the lease query (part of the first
    pass's fix) left crashed/hung executions never accumulating
    attempts via lease-expiry cleanup, risking an endless retry loop
    for a poison entry; and further doc-comment corrections, including
    an assertion in `docs/release_gate_summary.md` that a regression
    was "confirmed pre-existing via `git stash`" — a check performed
    after the regressing commits were already on `main`, with nothing
    left to stash back to; it is a genuine regression, and was already
    fixed by the bounded synchronous wait mentioned above.
  - Third pass, on `run_queue_pump.go`'s original design (in-range for
    the durable run queue itself, not either fix round): the pump
    could Ack (delete) a durable queue entry whose call was never
    actually executed, only appended to another live, in-process
    owner's mailbox queue — reachable both from a self-inflicted
    lease-expiry race (the 30-second lease TTL is far shorter than a
    real LLM turn, and execution never renewed its lease) and from two
    distinct queued entries for one session being dispatched back to
    back within the same pump tick. Either way, the second dispatch's
    call was silently duplicated into the first's mailbox (re-run when
    the owner drained its queue) while its own durable row was wrongly
    deleted — data loss if the process crashed before the mailbox
    drained, a duplicate turn if it didn't. Fixed by tracking which
    sessions this pump instance currently has an execution in flight
    for and refusing to lease further entries for the same session
    until it clears (closes the self-inflicted paths at the source),
    plus a distinct signal for the residual case of a genuinely
    external owner (neither Acked, since the work has not run, nor
    immediately retried, since that would itself append another
    duplicate — left leased for the existing lease-expiry mechanism to
    naturally recover).
  - Fourth pass: the third pass's fix above closed concurrent
    duplicate dispatch but not SEQUENTIAL duplicate dispatch — the
    30-second lease TTL is far shorter than a real LLM turn, and
    without lease renewal the durable row could flip back to pending
    while its own execution was still genuinely running; the eventual
    Ack would then silently fail to match, leaving the row for a LATER
    tick to lease and dispatch again — a real duplicate execution on
    any turn longer than 30 seconds, i.e. most real turns. Fixed with
    a lease-renewal loop that keeps a long-running execution's row
    genuinely leased for its whole duration. Separately, the third
    pass's "leave it leased, do nothing" handling for the
    externally-owned case relied on the same lease-expiry cleanup that
    unconditionally counts an attempt on every recovery — a session
    that stayed externally busy for a few minutes would have its
    accepted, never-actually-failed work silently deleted once
    attempts exhausted, the same class of bug the OS-lock-contention
    case was already protected against. Fixed via an immediate,
    no-attempt-penalty release paired with a local backoff deadline
    that prevents this pump instance specifically from re-attempting
    the same session too soon (a different process remains free to try
    immediately). A first attempt at this second fix (a single lease
    renewal call) was tried and found not to work by an isolated debug
    test before the real fix was written — the renewal landed
    essentially the same deadline leasing had already set, since it
    fired almost instantly after the lease was taken.
  - Fifth pass: the four queries that finalize a leased entry (ack,
    two kinds of release-back-to-pending, terminal-fail) matched on
    the row's id and leased status alone, with no check that the
    caller was still the current lease holder. If an executor ever
    lost its lease to a recovery (rare after the fourth pass's
    renewal loop, but not impossible under a severe scheduling stall),
    its late-arriving Ack or Nack could silently mutate or delete a
    row a different, currently-live executor now owned. All four
    queries are now scoped to the current lease holder. The reviewer's
    own verdict on this pass: the underlying concurrency design is
    settled, and this was the last targeted follow-up needed rather
    than the start of another full review round.

- **Three more bugs found while pushing and monitoring CI for the whole
  #337-349 batch** (this range had accumulated 39 unpushed commits and
  had not been tested against a genuinely clean environment until this
  push) — all confirmed as real via independent local reproduction and
  revert-checks, not accepted from CI's report alone:
  - **A forced app shutdown could leak the database file handle for the
    rest of a test binary's process lifetime**, breaking Go's own
    `t.TempDir()` cleanup on Windows ("process cannot access the
    file"). `App.Shutdown()`'s forced-shutdown path (taken whenever an
    agent doesn't finish within its grace period) intentionally skips
    releasing the database — correct for a real CLI/server process,
    which exits immediately after and lets the OS reclaim the handle —
    but a test helper that paired one `db.Connect` with exactly one
    `db.Release` assumed Shutdown always contributed its own share of
    releases, silently leaving the pool's reference count above zero
    forever whenever a test deliberately exercised the forced-shutdown
    path. Fixed with a new `db.ReleaseAll(dataDir)`, scoped to just
    that data directory's own pool entry, that guarantees teardown
    regardless of how many releases Shutdown's own policy performed.
  - **Building the coordinator could fail with "coder agent
    configuration is missing"/"coder agent not configured" outside of
    a normal config-file load** — `config.Load`/reload only populate
    the coder agent's config entry when a provider was already
    configured at the exact moment they run; several tests (and,
    structurally, any future caller) that configure a provider
    programmatically after an initial empty load bypass that entirely.
    Fixed by making both `App.InitCoderAgent` and
    `agent.NewCoordinator` self-heal: derive the missing config on the
    spot (a cheap, side-effect-free operation) instead of requiring
    every caller to remember to trigger it themselves.
  - **The same class of gap for the selected large/small model** — a
    handful of coordinator tests configured a provider but never
    selected a model to use with it, silently relying on the same
    leftover state the previous bug's fix now closes off. Unlike the
    coder-agent-config gap, this one is not self-healed in production
    code (correctly re-deriving a default model selection needs a
    resolved provider registry, which risks reintroducing a
    network-dependent test hang this codebase has hit before) — fixed
    by giving each affected test its own explicit model selection
    instead.

- **Eleven findings from a full-project `crush run --role reviewer`
  audit, closed and independently verified** (2 security blockers, 4
  bugs, 5 code-smell items with a code change — see the full report
  referenced from `docs/checkpoints/2026-08-11-0805.md`). Unlike prior
  rounds, this sweep covered the whole repository rather than one
  subsystem: `internal/cmd`, `internal/agent/cliprovider`,
  `internal/agent/tools`, `internal/config`, `internal/permission`,
  `internal/csync`. Every finding was independently re-verified by
  reading the actual code before acting, and every code change shipped
  with a genuine revert-check (temporarily undone, confirmed the exact
  predicted failure, restored, confirmed pass again).
  - **`env`/`nice`/`nohup`/`time`/`timeout` in the bash tool's
    "safe read-only command" allowlist let a model bypass the
    permission prompt entirely** — these are command-wrappers that run
    an arbitrary subcommand (`env rm -rf ./secrets`, `timeout 10
    ./exfil.sh`), so matching the allowlist's prefix check skipped
    `permissions.Request()` completely, defeating `--restrict-run`'s
    deny-by-default guarantee. All five removed from the allowlist
    (`printenv` kept as the safe alternative to bare `env`).
  - **The same wrapper class bypassed the sub-agent recursion guard
    too** — `agentguard`'s strip-list only recognized `exec`/`command`/
    `time`/`nohup`, so `env claude -p ...`, `nice claude ...`, `timeout
    30 claude ...` all slipped past it, and combined with the bug
    above, a single command could bypass both the permission system
    and the recursion guard at once. Added flag-aware stripping for
    `env`/`nice`/`timeout` (they take their own leading flags/
    positional args before the real command, unlike the pre-existing
    bare wrappers).
  - **A failed or cancelled `download` left a truncated file on disk
    with no size limit on the transfer** — a later `view`/`edit` would
    read the partial file as valid content. Rewritten to a temp-file +
    atomic-rename pattern (matching `fsext.AtomicWriteFile`'s existing
    convention) with a new 500 MiB cap.
  - **A CLI-provider session's MCP server registration could be
    deleted out from under a still-running concurrent session in the
    same project** — Gemini/Qwen MCP registration uses a stable
    per-project server name by design (survives process restarts), but
    the matching deregister-on-exit deleted that entry unconditionally
    even if a newer session had since overwritten it with its own
    endpoint. Deregister now compares against the exact value its own
    registration wrote and is a no-op if someone else now owns the
    entry.
  - **Two attachments with the same filename in one CLI-provider
    request silently overwrote each other on disk** — both message
    entries kept pointing at the same, last-write-wins file. Fixed with
    filename-collision disambiguation, mirroring the pattern the web
    server's own attachment handler already uses.
  - **The max-cost/max-tokens budget-cap enforcement's turn-stopping
    mechanism turned out to rely on an undocumented, easy-to-break
    invariant** — investigated as a possible fifth bug, but confirmed
    (independently, twice) to be structurally unreachable in the
    current code: the internal map that budget-cap's abort path reads
    a cancel function from is populated before the turn's first
    callback can ever run, and nothing in the codebase ever clears that
    entry early. Not an active bug today, so left as a pinned regression
    test plus a warning comment instead of restructuring sensitive
    turn-lifecycle code for a scenario that can't currently occur.
  - **The Qwen CLI MCP integration used a 32-bit-entropy stable ID as
    its auth token, passed via a `?token=` URL query parameter that
    gets logged** — justified in code as "qwen doesn't support custom
    headers." Re-checked against the actually-installed qwen CLI
    package and found that claim outdated: it does support a `headers`
    field on HTTP MCP servers. Switched to `Authorization: Bearer`
    with a full 256-bit random token, matching the Claude/Gemini/Codex
    integrations.
  - **The MCP server's built-in tools (Bash/Read/Write/Glob/Grep) each
    generated two unrelated UUIDs per call** — one for internal event
    tracking, one for the permission request — leaving the permission-
    notification stream uncorrelated with the tool-event stream for
    the same call. Unified to one ID per call, matching the pattern the
    external-MCP-tool handler already used.
  - **`download`/`fetch` had no defense against a model (directly, or
    via a prompt-injected web page) requesting an internal or
    cloud-metadata URL** (e.g. `http://169.254.169.254/latest/
    meta-data/`) and leaking the response into `final_text` — the only
    prior gate was the permission system, which auto-approves in
    non-interactive `crush run`/web contexts. Added a shared SSRF guard
    that blocks loopback/private/link-local/metadata destinations at
    dial time (`net.Dialer.Control`, which sees the actually-resolved
    IP after DNS lookup but before connect — closes both the trivial
    bypass and DNS-rebinding). Callers that legitimately need loopback
    (local dev, self-hosted) inject their own client via the tools'
    existing `*http.Client` constructor parameter.
  - Two smaller findings closed without behavior risk: a `go vet`
    warning about a value-receiver copying a mutex was investigated and
    left as the existing, correctly-suppressed no-op (the "fix" would
    have broken config JSON-schema generation or config-file loading,
    depending on which half of the conflict was resolved); an unchecked
    type assertion on a `singleflight` result was switched to the
    comma-ok form; a copilot token refresh gained a 30s timeout; three
    session-permission CRUD methods (currently unused in production,
    reserved for a future web UI surface) now accept a caller `ctx`
    instead of using `context.Background()` internally; a misplaced
    doc-comment was moved to the function it actually documents.

  **Closing independent review of this round's own fixes** found 1
  blocker and 3 bugs in the fixes above — all confirmed and closed the
  same way, with revert-checks on the security-relevant ones:
  - **The new SSRF guard only covered 2 of 6 model-facing HTTP tools**
    — `download`/`fetch` were guarded, but `agentic_fetch`, `web_fetch`
    (which `agentic_fetch`'s spawned sub-agent gets, and which
    explicitly skips the permission system), `web_search`, and
    `sourcegraph` each built their own unguarded client, leaving the
    exact exfiltration path the guard was supposed to close still open
    through those four. All six now share the same guarded default.
  - **The guard's documented "inject a client with `allowPrivate=true`"
    escape hatch didn't actually exist anywhere in production wiring**
    — a routine local-dev fetch (`http://localhost:3000/...`) was
    permanently blocked with no way to restore it short of a code
    change. Added an off-by-default `allow_private_network_fetch`
    config option, wired through to all six guarded tools.
  - **The guard was bypassable via `HTTP_PROXY`/`HTTPS_PROXY`** — the
    dial-time check only ever sees the address actually dialed, which
    is the proxy's address when one is configured, not the request's
    real target. The guarded transport now disables proxying outright.
  - **The attachment-filename-collision fix (above) still collided on
    a mixed input set** — e.g. `["image.png", "image.png",
    "image-1.png"]`: the second `image.png` disambiguated to
    `image-1.png`, and the third, literal `image-1.png` then silently
    reused that generated name. Fixed by tracking the set of names
    actually written instead of a per-original-name counter.
  - **The download atomic-write fix (above) silently dropped the
    downloaded file's permissions** from world-readable to owner-only
    (`os.CreateTemp`'s default `0600`, never chmod'd back up before the
    rename, unlike the codebase's own `fsext.AtomicWriteFile`
    convention it was modeled on).

- **Fifteen findings from a 2026-08-11 release-readiness concurrency
  review, closed and independently verified** (3 release blockers, 5
  high-priority, 6 medium-priority items — see
  `docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md`).
  This round covers the core session/mailbox/durable-queue lifecycle,
  the same subsystem seven prior review passes already hardened; every
  fix was implemented via a delegated `/crush` session and then
  independently re-verified: the diff read line by line, every new test
  checked for whether it would actually fail without the fix, and every
  safety/concurrency-critical fix personally revert-checked (temporarily
  undone, confirmed the exact predicted failure, restored, confirmed
  green again). That verification pass itself found real, non-trivial
  bugs in most of the delegated diffs, listed inline below.
  - **A durable-queue call could execute twice after handoff into a
    busy session's mailbox** — `mailbox.submit` unconditionally
    appended durable-originated calls to the in-memory queue even
    though the durable row itself is already the retry path, so the
    live owner would run the in-memory copy and the pump would
    independently re-lease and run the same durable row after backoff.
    Calls originating from the durable queue no longer get a second,
    redundant in-memory queue entry.
  - **An "accepted" call — an idle interrupt, or orphaned mailbox work
    found during release — could still be lost before its durable-queue
    commit, with no error surfacing anywhere** — `startDetachedRun` and
    `restartOrphaned` both silently swallowed enqueue failures. Both
    now propagate the failure to their own callers instead of
    discarding it; for the cross-process interrupt-inject path, a
    failed durable enqueue now recreates the `pending_injects` row for
    a future retry instead of vanishing.
  - **`RunQueuePump.Stop()` returned before in-flight `executeEntry`
    workers finished**, so a shutdown could drop an Ack/Nack that was
    mid-flight and cause the same durable row to be re-run after
    restart. `Stop()` now joins a `workerWg` covering every dispatched
    worker (5-second grace, matching `CancelAll`'s own pattern), guarded
    by an admission gate that closes the same class of "check state,
    then start work" race the fix itself was designed to prevent —
    found in the delegated diff's own new code during verification.
  - **`CancelAll`'s shutdown join didn't cover manual `Summarize()`
    calls or the title-generation goroutine** — a summarize/title
    operation still writing to the database after `CancelAll` returned
    could race a subsequent session teardown. Both are now covered by
    the same `runWg` `CancelAll` already joins, via an admission gate
    (`admitMu`/`shuttingDown`) that closes the same
    check-then-`Add` race pattern found in the RunQueuePump fix above —
    Go's `sync.WaitGroup` forbids a concurrent `Add(positive)` starting
    from zero unless it happens-before a concurrent `Wait()`, and an
    unsynchronized "check shutting-down, then Add" is a real bug even
    when each half looks correct in isolation.
  - **A lease lost during execution didn't stop the in-flight,
    non-idempotent LLM/tool work still running under it** — the
    execution context stayed uncancellable after a lease-loss recovery
    reassigned the row to a new owner, so both the original executor
    and the new owner could run the same call concurrently. The
    execution context is now cancelled the moment lease loss is
    detected, and outcome writes (Ack/Nack/terminal-fail) are skipped
    once that happens rather than racing the new owner's own writes.
  - **FIFO ordering for the durable queue was undefined for two calls
    enqueued within the same second** — `created_at` alone isn't a
    stable sort key at one-second resolution. Added an implicit-`rowid`
    tie-breaker to both scan queries (SQLite gives every non-`WITHOUT
    ROWID` table a free, already-monotonic rowid — no migration
    needed).
  - **The pump's fan-out after a backlog or restart was unbounded** — a
    large recovery backlog could dispatch unlimited concurrent
    `executeEntry` workers at once. Added a bounded semaphore (10
    concurrent by default, test-overridable) that every dispatch path
    acquires before leasing and releases on every exit.
  - **Async lock-metadata cleanup could clobber a new, live owner's
    PID** — after a slow `Release()`'s best-effort cleanup goroutine
    outlives its bound, it could still overwrite a `.pid` sidecar a
    brand-new owner had since written. Added a generation-versioned
    `.gen` sidecar; cleanup now compares its recorded generation
    against the current one and skips only on a genuine positive
    mismatch (a missing or unreadable sidecar still falls back to the
    pre-fix unconditional cleanup, closing a regression the delegated
    diff's first attempt introduced by treating "missing" the same as
    "mismatched").
  - **The durable-queue "idempotency key" was actually a fresh unique ID
    on every call, not a stable retry key** — generated from
    `time.Now().UnixNano()` each time, so a caller-level retry of the
    same logical request produced a different key and could create a
    second durable row for what should be one logical call. Added a
    `LogicalCallID`, generated once when a call is first constructed,
    and keyed idempotency off `SessionID+LogicalCallID` (falling back to
    the old timestamp behavior only if unexpectedly empty). Making the
    key genuinely stable exposed a second gap the naive fix alone would
    have made worse — `EnqueueRunQueueEntry`'s insert had no conflict
    handling at all — closed with `ON CONFLICT(id) DO NOTHING`.
  - **`agentguard`'s sub-agent recursion guard was still bypassable via
    `env -S`/`--split-string`**, and `CheckWindowSafety` used a
    separately-maintained wrapper list that had already drifted from
    the main guard once before (the exact class of bug a prior review
    round's `env`/`nice`/`timeout` fix closed for the primary path
    only). `env -S`/`--split-string` payloads are now recursively
    re-checked, and both guards now share one `resolveCommandHead`
    helper so the wrapper-stripping logic can't diverge between them
    again.
  - **The Codex CLI provider's MCP token was still exposed via a
    `?token=` URL query parameter**, visible in argv/
    `/proc/<pid>/cmdline` — unlike the Gemini/Qwen providers, which
    already use an `Authorization` header. Moved to an environment
    variable referenced through Codex's own `bearer_token_env_var`
    config key, verified against the actually-installed `codex` CLI
    (not assumed from documentation) to confirm the mechanism works and
    is never persisted to `~/.codex/config.toml`.
  - **The SSRF guard's range denylist missed several IANA
    special-purpose ranges** not covered by any Go stdlib `net.IP`
    method — CGNAT (`100.64.0.0/10`), the IPv4 benchmark range
    (`198.18.0.0/15`), the TEST-NET-1/2/3 documentation ranges, the
    reserved `240.0.0.0/4` block, and IPv6 equivalents (discard-only,
    documentation, ORCHIDv2). Added a static CIDR table checked
    alongside the existing loopback/private/link-local/unspecified
    checks, with 42 new boundary test cases.
  - **The mailbox's ownership-exit finalizer could hand a newer era's
    queued work to the wrong recovery path** — releasing ownership and
    draining any leftover queued work were two separate lock
    acquisitions, leaving a window where a new owner's own queued call
    could be misrouted into detached recovery instead of staying for
    that owner's normal end-of-turn drain. Both steps are now one
    atomic critical section.
  - **Recovery-subsystem architecture cleanup**: audited which of six
    documented complexity/drift smells in the run-queue pump were
    already resolved by the fixes above before touching anything (per
    this task's own explicit instruction) — three were already closed,
    two were genuinely still open and fixed (an unused
    `RunQueuePumpConfig.DataDirectory` field removed; the pump's
    "executed entry successfully" log no longer prints unconditionally
    when the following database ack actually failed), and two are
    flagged as legitimate future-scoped work rather than force-fit into
    a mechanical cleanup pass (the file's line growth from this round's
    own correctness fixes; a `terminal_failure` column that is
    currently dead state, since a terminal failure deletes the row
    rather than ever setting the flag — closing it properly needs a
    schema migration on the exactly-once durable queue this whole round
    protects, judged out of scope here).

- **Two UI bugs: switching a model in the web UI mutated global config
  instead of the current session** — reported live during this round.
  Model resolution is now a pure function of `(session, config)` instead
  of mutating shared agent state, and the global-default vs
  session-scoped model APIs are split, fixing the server call sites that
  were building a system prompt from the wrong (globally-mutated) model.

  **Closing independent review of this round's own fixes** (an
  18-commit range) returned a NO-GO with one build failure and one new
  release blocker, both introduced within the round itself — all
  confirmed and closed the same way, with a personal revert-check on
  the concurrency-relevant one:
  - **`internal/agent` did not compile under `go test`** — the
    recovery-subsystem cleanup above (the `DataDirectory` field
    removal) was verified only against the two `internal/session`
    packages it touched, missing ten more call sites in
    `internal/agent` that also set the now-removed field. Every
    regression test this round produced under `internal/agent` had
    not actually run since that commit landed. All ten sites fixed;
    `go vet ./...` is now clean except one pre-existing, unrelated
    finding.
  - **A long-running turn could be executed twice** — the very
    `RunQueuePump.Stop()` fix earlier in this round introduced a
    single 30-second database-write context created before
    `Coordinator.Run` was even called, then reused for both lease
    renewal and the final outcome write. Any real turn longer than 30
    seconds silently stopped renewing its lease and then failed to
    Ack/Nack with "context deadline exceeded," leaving the row
    leased/pending for the next tick to re-lease and re-execute —
    exactly the duplicate-execution bug that `Stop()` fix exists to
    prevent, reintroduced by the same commit. Fixed by minting a fresh
    write budget per individual database call instead of one shared,
    prematurely-started context; the renewal loop's own lifecycle is
    now independent of that budget so it survives the whole turn.
  - **A second instance of the mailbox era-boundary reordering gap**
    (see the mailbox finalizer fix above) was found in a code path the
    original fix didn't touch — the manual-compaction success path
    that hands off to the next queued summarize request. Closed with
    the same atomic-pop pattern as the original fix.

- **Ten findings from a 2026-08-12 post-fix release-readiness review,
  closed and independently verified** (3 release blockers, 4
  high-priority, 1 security, 1 medium-priority, plus one
  cross-branch coordination gap found during the merge itself — see
  `docs/reviews/2026-08-12-post-fix-release-readiness-review.md`).
  Genuinely-independent work was parallelized across git worktrees
  (one branch per finding) and merged back after each was individually
  verified; every fix went through the same zero-trust process as the
  prior round — diff read line by line, every new test checked for
  whether it would actually fail without the fix, every
  concurrency/correctness-critical fix personally revert-checked. Real,
  non-trivial defects were found and closed in the majority of the
  delegated diffs, listed inline below.
  - **A durable-queue call's idempotency key (`LogicalCallID`) was
    dropped across the JSON round-trip through the run-queue's
    serialized call data**, so a retried call landed with a fresh,
    non-idempotent key and could execute twice. `LogicalCallID` is now
    carried through both directions of the conversion
    (`ToSessionAgentCallData`/`FromSessionAgentCallData`) and
    `RebuildSessionAgentCall`.
  - **Cross-process interrupt handoff deleted the pending-inject row
    before the durable enqueue that was supposed to replace it was
    guaranteed to succeed** — any fallible step in between (message
    read, model/call build, marshal, enqueue) lost the event
    permanently once the row was gone, since the next tick had nothing
    left to retry. `ConsumeInterruptInjectAndEnqueue` now deletes and
    durably enqueues in one SQL transaction; failures before that point
    simply leave the still-undeleted row for the next tick to retry
    naturally. Verification found the delegated diff's own atomic
    query re-selected "the oldest pending row" instead of matching the
    specific row a caller had peeked and built its call data from — a
    session with more than one queued interrupt could silently consume
    and lose the wrong one if a concurrent path deleted the peeked row
    in between. Fixed to match on the exact peeked row ID, returning a
    safe no-op instead of a wrong-row substitution when it has vanished.
  - **The checkpoint writer's cancellation was dead code** — the
    per-cycle write context's cancel function was only ever invoked via
    `defer` inside its own goroutine (a no-op against a hang, since the
    goroutine that would call it is the one that's stuck), so
    `stopCheckpoint` had no real way to interrupt a blocked write and
    fell through to a 5-second grace wait every time regardless of
    whether the write was actually still live. The cancel function is
    now stored on the enclosing scope and called directly from
    `stopCheckpoint`, immediately unblocking a genuinely stuck write.
  - **Orphan recovery still had a runnerless fallback** — when durable
    enqueue failed for a call recovered during ownership handoff, the
    old fallback queued it into a (potentially idle) in-memory mailbox
    that only a future, unrelated `Run()` would ever drain; if the
    session was truly idle, the call was lost with no further trace.
    Replaced with a bounded, lifecycle-joined detached run
    (`startBoundedDetachedRun`, 30s timeout, tracked in the same
    `runWg` `CancelAll` already joins) that actually attempts the call
    in-process instead of merely queuing it, with all failures logged
    at ERROR level. Documented as a best-effort improvement, not a
    durability guarantee — a process crash during the bounded window
    still loses the call, since durable enqueue (the actual durable
    path) already failed to get there in the first place.
  - **`RunQueuePump.executeEntry`'s lease-renewal loop only reacted to
    an explicit "lease reassigned" response**, not to renewal calls
    that simply stopped succeeding (a DB outage or network partition) —
    execution could keep running real, non-idempotent work indefinitely
    after another process had already taken over the same lease. Added
    a fail-closed timeout: if no renewal has succeeded within a full
    lease TTL, execution now cancels itself the same way an explicit
    lease-loss response does. Documented the durable queue's
    at-least-once (not exactly-once) semantics for persistent side
    effects and the residual overlap window this timeout bounds but
    does not eliminate.
  - **The session-model cache had no invalidation path** — introduced
    as a regression by an earlier round's per-session model-isolation
    fix, a config change or credential refresh (401 retry) left
    `resolveSessionModels`'s cache silently serving a stale
    model/provider pairing. Cache keys now fold in a monotonic config
    generation counter, and both `UpdateModels` and the 401-retry path
    explicitly clear the cache.
  - **Two cross-process interrupt call sites (`requeueInterruptMessage`,
    `InterruptAndSend` without explicit overrides) still fell back to
    the shared/global model instead of a session's own persisted
    override** when handed a nil model snapshot — the exact isolation
    gap an earlier round's fix was supposed to have closed everywhere.
    `buildCall`/`runInternal` now reject a nil model snapshot outright
    instead of silently substituting shared state, and both call sites
    resolve session models first. Merging this fix's branch against
    this round's separately-landed atomic interrupt-handoff rewrite
    (above) surfaced a third call site with the identical bug,
    introduced by that same rewrite after this fix's scope had already
    been fixed — closed the same way before the merge landed.
  - **The CLI provider logged the full prompt text and raw argv at INFO
    level** by default, including any secrets embedded in a prompt.
    Both are now redacted unless
    `CRUSH_CLIPROVIDER_LOG_RAW_PROMPT=1` is explicitly set for
    diagnostics.
  - **The lock-metadata generation guard's residual gap** (a narrow
    window where async cleanup can still race a brand-new owner despite
    the generation check) is now documented as an explicit accepted
    risk — citing the prior reverted architectural attempt and
    `sessions kill`'s own re-probing safety net — rather than left as
    an ambiguous "known gap" comment.
  - **`RunQueuePump.Stop()`'s wait on the main polling loop was
    unbounded** — only the worker-drain half of `Stop()` had a
    deadline; a stuck main loop could hang shutdown indefinitely.
    `Stop()` now bounds both halves to a single shared 5-second budget
    (not 5+5=10s) via one timeout context and a three-way select.
