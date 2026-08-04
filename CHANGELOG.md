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
  `sessions_why_test.go`, `stream_watchdog_test.go`, and
  `internal/session/lock_test.go` used a 1-second timing margin around
  `MaxPidFallbackAge` that was too tight under load; widened to 2 minutes.
  A separate stream-watchdog test whose 100ms timing budget went stale
  once its diagnostic capture became synchronous by design now uses a
  direct file-presence check instead of a timing budget that a widened
  timeout would have made unable to catch its own regression. Also fixed a
  read-before-write race in the same watchdog test (polled for a dump
  file's existence rather than its content, occasionally reading it
  mid-write) and made a probabilistic ENOENT-race regression test
  deterministic via a test-only hook instead of a goroutine race that could
  pass even against a reverted fix.
