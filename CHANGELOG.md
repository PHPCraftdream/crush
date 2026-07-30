# Changelog

User/operator-facing changes to this fork. Not to be confused with
`CHANGELOG.fork.md`, which tracks upstream-merge decisions for future
mergers — this file tracks what actually changed in behavior.

Format loosely follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Fixed

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
