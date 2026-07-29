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
