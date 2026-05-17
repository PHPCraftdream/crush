# CHANGELOG.fork.md

This document tracks every divergence between this fork and upstream
`charmbracelet/crush`. Its purpose is **not** to be a release changelog
for end users — it is a survival guide for the next person (likely
ourselves) who has to merge upstream `main` into the fork and decide,
file by file, which side to keep.

When you finish a merge, append a new entry to the **Chronological
commit log** section at the bottom. When you make a non-obvious patch to
an upstream file, leave a `// Fork patch:` comment in the code pointing
back here.

## 1. Why this fork exists

Upstream Crush is a **terminal UI** (TUI) coding agent. The optional
`client/server` mode in upstream still drives the TUI — its REST API
under `/v1/...` is just a transport between the TUI front-end (running
in a separate terminal) and the agent back-end (running on a Unix socket
or Windows named pipe).

This fork has a different goal: **run Crush as a long-lived embedded web
service**, with a React/Tailwind UI in the browser. Everything we kept,
removed, or rewrote follows from that single choice.

### High-level differences from upstream

| Area | Upstream | This fork |
| ---- | -------- | --------- |
| Front-end | Bubble Tea TUI (`internal/ui/`, ~495 files) | React SPA (`web/`, embedded via `go:embed`) |
| Transport | REST `/v1/...` over Unix socket / Windows named pipe (`internal/server/proto.go`, 30KB) | WebSocket `/ws` over TCP loopback (`internal/server/handlers.go`, ~58 handlers) |
| Auth | None (local-socket trust) | Token-based, see `internal/server/auth.go` |
| Workspace model | `internal/workspace/` abstraction (single local vs. remote-HTTP impl behind `CRUSH_CLIENT_SERVER` env) | Single embedded `appPkg.App`; no abstraction (workspace package removed) |
| Sessions | One model per agent role, set globally | Per-session model overrides stored in DB (migration `20260308000001`) |
| Permissions | In-memory rules during a TUI run | Persistent per-session rules in DB (migration `20260308000002`) |
| Skills | TUI list dialog | `web/` slash-command autocomplete + browser dialog |
| CLI providers | `internal/agent/cliprovider/` is upstream too but limited | Heavily extended: `npx @anthropic-ai/claude-code`, Gemini/Codex CLIs, MCP bridge, session resume for prompt caching |

### Things we are NOT doing

- We do not implement upstream's REST `/v1/...` API. The fork's
  WebSocket protocol is not wire-compatible with it. A future migration
  is technically possible (see prior analysis in commit history), but
  not planned — the WebSocket is fine.
- We do not ship the TUI. Any merge conflict that resurrects
  `internal/ui/<anything-but-notification>` should be resolved by
  deleting the file on our side.
- We do not preserve `flake.nix` / `.envrc` / Hyper subscription paths
  from upstream — see Section 3.

## 2. Merge guidance — how to resolve conflicts from upstream

When you run `git merge origin/main` and get conflicts, the table below
tells you which side is authoritative for each common conflict class.

| Conflict pattern | Side to keep | Why |
| ---------------- | ------------ | --- |
| `internal/ui/anim/`, `internal/ui/chat/`, `internal/ui/common/`, `internal/ui/completions/`, `internal/ui/dialog/`, `internal/ui/list/`, `internal/ui/logo/`, `internal/ui/model/`, `internal/ui/styles/` (DU — we deleted, they modified) | **Delete** (`git rm`) | TUI was removed in `7ff2292e`. Resurrecting any of it pulls in a tree of bubbletea/lipgloss imports our `cmd/web` doesn't use. |
| `internal/ui/notification/` | **Keep** | OS-native notifications are reused by the web server. |
| `internal/backend/` (DU) | **Delete** | Upstream's split between agent core (`backend`) and TUI was meant for the client/server mode. We have a single embedded `appPkg.App` in `internal/app/` so there is no need for the split. |
| `internal/client/` (DU) | **Delete** | HTTP REST client for upstream's client/server mode. Our front-end is the browser, not Go. |
| `internal/cmd/server.go`, `internal/cmd/server_*.go`, `internal/cmd/session.go`, `internal/cmd/logout.go`, `internal/cmd/clientserverrace/` (DU) | **Delete** | Upstream client/server CLI subcommands. Our subcommand is `crush web` (see `internal/cmd/web.go`). |
| `internal/server/proto.go`, `internal/server/config.go`, `internal/server/logging.go`, `internal/server/net_*.go` (DU) | **Delete** | Upstream's REST `/v1/...` server. Our server is `internal/server/{server,handlers,protocol,events,hub,auth,wire}.go` and lives next to them. |
| `internal/workspace/` (DU) | **Decide each merge.** Last time (commit `8b30fad1`'s merge resolution) we silently dropped it. Currently we don't import the package anywhere, so the build passes either way. If upstream starts hardening the abstraction in a way we want to inherit, restore it — otherwise leave deleted. Document the decision in the commit message. |
| `go.mod` / `go.sum` (UU) | **Merge both sides.** Take upstream's version bumps but keep our additions: `@rsbuild/plugin-babel`-related Go deps are none (frontend-only), but anything we added for the web server (websocket/cors/etc) must stay. Run `go mod tidy` after resolving. |
| `internal/cmd/root.go` (UU) | **Keep ours but pull in their new flags.** We register `crush web` as the default subcommand; upstream registers `tui`. Diff carefully and copy over only flag additions. |
| `internal/cmd/login.go` (UU) | **Keep ours.** Our flow stores tokens for the web server; upstream's writes to the TUI auth context. |
| `internal/agent/agent.go` (UU) | **Merge by hand.** This is our main extension point: sliding context window, queued compact, silent background compaction, empty-response detection (see Section 4.A). Upstream evolves this file too. Take each new upstream change as a separate patch and verify our `// Fork patch:` blocks still apply. |
| `internal/agent/coordinator.go` (UU) | **Merge by hand.** We added `TakeSummarizeQueue`, `RunWithOverrides`, and per-session yolo/permission tracking. Upstream's coordinator drives one TUI session at a time, ours drives N concurrent web sessions. |
| `internal/message/message.go`, `internal/message/content.go` (UU) | **Merge by hand.** We added fields `Pinned`, `Hidden`, `ReasoningEffort` (with DB migrations). Keep both sides' new fields; matching DB migrations live in `internal/db/migrations/20260*`. |
| `internal/permission/permission.go` (UU) | **Merge by hand.** We persist permissions per-session in the DB. Upstream keeps them in memory. Our persistence layer must survive any refactor. |
| `internal/config/config.go`, `internal/config/load.go`, `internal/config/store.go` (UU) | **Merge by hand.** Our `Hooks` configuration block must remain (see `docs/hooks/README.md`). |
| `internal/version/version.go` (UU) | **Merge by hand.** Take their version bump, keep our git-info exposure if changed. |
| Any new `internal/proto/*.go` from upstream (added/modified) | **Keep upstream.** This is the new typed wire protocol they introduced; we do not use it yet but it is read-only types — leaving it in does not hurt and lets us adopt it incrementally. |

When in doubt: **prefer keeping our side** for anything that wires into
the WebSocket server or the React UI; **prefer upstream's side** for
anything in `internal/agent/tools/*` (tool implementations), provider
catalogs (`internal/agent/hyper/`), and `schema.json` (JSON-schema for
config). The latter we want to track upstream as closely as possible
so users' editor configs keep working.

## 3. What was removed from upstream

Removals fall into five buckets. The bucket is the reason we removed
them, not the file type.

### 3.A — TUI (~495 files in `internal/ui/`)
Replaced by `web/` (React + Tailwind, embedded with `go:embed all:dist`
from `web/embed.go`). Initial removal in `7ff2292e`.

The only `internal/ui/` directory we kept is `internal/ui/notification/`
— it implements OS-native notifications (Windows toast / macOS
NotificationCenter / Linux libnotify) and is reused by the web server
to surface re-auth and out-of-credits events to the user.

### 3.B — Upstream's client/server REST layer
- `internal/server/proto.go` (REST `/v1/...` handlers — ~30KB)
- `internal/server/config.go`, `internal/server/logging.go`,
  `internal/server/net_other.go`, `internal/server/net_windows.go`
- `internal/client/` (Go client of the REST API)
- `internal/cmd/server.go`, `internal/cmd/server_other.go`,
  `internal/cmd/server_windows.go` (CLI plumbing)
- `internal/cmd/session.go`, `internal/cmd/clientserverrace/`,
  `internal/cmd/spawnlock_*.go` (TUI ↔ daemon coordination)

Our replacement: `internal/server/{server,handlers,protocol,events,
hub,auth,wire}.go` — a WebSocket server with token auth, mounted at
`/ws` with the React bundle served at `/`.

### 3.C — `internal/backend/` package
Upstream split agent-core (`backend/`) and TUI-front (`ui/`) so the
client/server mode could plug HTTP between them. We embed everything in
one process under `internal/app/App`, so the split is unnecessary churn.

Removed files: `agent.go`, `backend.go`, `config.go`, `events.go`,
`filetracker.go`, `permission.go`, `session.go`, `util.go`.

### 3.D — `internal/cmd/logout.go` + `login_test.go`
Upstream's logout was tied to its OAuth-for-Hyper flow with on-disk
credential cache files. We removed the subcommand because the web UI
handles provider credentials directly (Settings → Providers). The
underlying `internal/oauth/` packages stay — we just don't expose a
shell-level logout command.

### 3.E — Telemetry / metadata files
- `flake.nix`, `flake.lock`, `.envrc` — Nix dev environment we don't
  use on Windows.
- `INVESTIGATION.md` was upstream's pre-release notes — replaced by
  this document.
- A handful of `*.md.tpl` files were renamed back to `*.md` because we
  removed the templating step that upstream introduced and never used.

## 4. What was added in this fork

Topical groups (not chronological — see Section 6 for that).

### 4.A — WebSocket server (`internal/server/`)

Files added (all new — none from upstream):

| File | Role |
| ---- | ---- |
| `server.go` | HTTP mux: `/auth`, `/auth/check`, `/ws`, `/` (SPA). Token auth. CORS for local dev. |
| `handlers.go` | 58 `handleXxx(ctx, a *App, c *Client, msg WSMessage)` handlers — one per WS message type. The fork's actual API surface. |
| `protocol.go` | WS message type names + payload structs (request side). |
| `events.go` | WS event type names + payload structs (server-push side). |
| `wire.go` | Envelope `{type, id, payload}` + JSON framing. |
| `hub.go` | Hub of connected clients, broadcast routing, per-session filtering. |
| `auth.go` | Token mint/verify (HMAC of timestamp+secret), stored in `~/.config/crush/token`. |

### 4.B — React web UI (`web/`)

Built with Rsbuild (React Compiler enabled). Mounted by `web/embed.go`
via `//go:embed all:dist`. See `web/README.md` (if it exists) for the
dev loop; otherwise `cd web && npm run dev` for the dev server and
`npm run build` to produce `web/dist/`.

Key components (`web/src/components/`):
- `Chat.tsx`, `Message.tsx` — message list + per-message rendering.
- `ChatInput.tsx`, `ChatToolbar.tsx` — input + actions.
- `Sidebar.tsx`, `Header.tsx`, `StatusBar.tsx` — chrome.
- `ModelSelector.tsx`, `ProvidersModal.tsx`, `SettingsModal.tsx`,
  `LSPSettings.tsx`, `MCPSettings.tsx`, `PermissionDialog.tsx`,
  `LogsModal.tsx`, `SubAgentBlock.tsx`, `TodoList.tsx` — feature panes.

Tests: `web/tests/*.spec.ts` (Playwright). Each top-level feature has a
matching spec.

### 4.C — Database extensions

New migrations under `internal/db/migrations/` (all dated 2026-03-08
through 2026-03-13):

| Migration | Adds |
| --------- | ---- |
| `20260308000001` | `large_model_*`, `small_model_*` columns to `sessions` (per-session model overrides). |
| `20260308000002` | `session_permissions` table. |
| `20260308000003` | `system_prompt` column to `sessions` (per-session prompt override). |
| `20260309000001` | Cleanup of empty permission rows produced by `20260308000002`. |
| `20260310000001` | `pinned` flag on `messages`. |
| `20260311000001` | `hidden` flag on `messages` (used by silent background summaries). |
| `20260312000001` | `yolo` flag on `sessions` (per-session, replaces the global flag). |
| `20260312000002` | `enabled` flag on permission rows (soft-delete-friendly). |
| `20260313000000` | `large_model_reasoning_effort` column on `sessions`. |
| `20260313000001` | `reasoning_effort` column on `messages`. |

Generated SQL in `internal/db/permissions.sql.go`,
`internal/db/sessions.sql.go`, `internal/db/messages.sql.go`. Source
queries in `internal/db/sql/*.sql`.

### 4.D — Agent / coordinator extensions (`internal/agent/`)

Patches and additions in `agent.go`, `coordinator.go`:

- **Sliding context window** — when used tokens approach the model's
  context window, the oldest messages are trimmed inside `PrepareStep`
  rather than blocking the user on a synchronous summarisation call.
  Introduced in `b899cb4c`.
- **Silent background compaction** — when the window slides, the oldest
  50% of messages are summarised in the background; the running task
  is not interrupted. Introduced in `107c5faa`.
- **Queued compact** — explicit user-triggered `summarize` while a
  task is in flight gets queued and executed when the task ends.
  Introduced in `b899cb4c`.
- **Always-continue after auto-summary** — after a summary, the agent
  resumes the user's original task instead of stopping. Introduced in
  `9886d533`.
- **Stuck busy-state recovery** — if a summarisation fails or runs too
  long, the session's busy flag is force-released so the user can keep
  typing. Introduced in `9eb7e5e5`.
- **Empty-response detection** — when a provider closes the stream
  without sending any content (no text, tool_call, or reasoning), we
  now record `FinishReasonError` with an explicit message instead of
  the previous `FinishReasonUnknown` with empty parts (which the UI
  rendered as a blank block). Added in agent.go's `OnStepFinish`. See
  also Section 4.G for the matching front-end fallback. Marker:
  `// Fork patch: surface empty-stream as a visible error.`
- **Stream-progress watchdog** (`internal/agent/stream_watchdog.go`) —
  every fantasy stream callback bumps an atomic activity timestamp;
  a watchdog goroutine cancels the generation context if no callback
  fires for `streamIdleTimeoutDefault` (3 min default), overridable
  per app via `Options.StreamIdleTimeoutSeconds` in `crush.json`.
  Adds the "Codec must surface control" invariant: a provider that
  holds the HTTP body open but stops sending bytes (rate-limit,
  HTTP/2 stall, backend hiccup) can no longer freeze the agent
  indefinitely. On fire, the assistant message gets a
  `FinishReasonError("Stream stalled")` with the duration so the user
  knows why their turn ended. Backed by the 162-promise-all
  post-mortem (D:\dev\garnet-team\.crush): four streams (parent + 3
  sub-agents) all froze in a 9-second window when a provider stopped
  responding mid-stream, and the agent waited 1.5h before the user
  killed the process. **Tune up to 10–15 minutes** (e.g.
  `"stream_idle_timeout_seconds": 900`) when running extended-thinking
  models (Opus 4.7 / Sonnet 4.5 with large thinking_budget) — a long
  reasoning gap can legitimately exceed 3 min.
- **Detached-context flush at the error path** — the final
  `messages.Update` that records the assistant's finish part now uses
  `context.WithoutCancel(ctx)` + a short timeout, instead of the
  outer ctx. Without this, Ctrl-C in `crush run` (which cancels the
  signal.NotifyContext that fang gives every subcommand) would
  propagate `ctx.Canceled` into `Update` and leave the assistant
  message half-saved: tool calls present, no finish part — the
  "silent dying" pattern. Same fix applied to all error-path
  `Create`/`Update`/`List` calls.
- **Startup recovery of orphan assistants**
  (`internal/app/app.go::recoverInterruptedTurns`) — on every app
  start, before the coordinator is wired up, scan all sessions; any
  assistant message left without a finish part from a previous run
  gets a `FinishReasonError("Process restarted")` so the WUI
  immediately renders it as an interrupted turn rather than spinning
  forever. Safety net for cases the in-process defences can't catch:
  `kill -9`, power loss, OS reboot, panic-without-recovery.

### 4.E — CLI providers (`internal/agent/cliprovider/`)

The fork ships substantially more CLI integration than upstream:

- `npx @anthropic-ai/claude-code` invocation (Windows-friendly NoPTY
  + StdoutPipe/StderrPipe, see `31592eff..a9897fca`).
- Claude CLI 1M-context models (`cli-claude-opus-1m`,
  `cli-claude-sonnet-1m`) wired as defaults for the `local-cli`
  provider.
- Gemini and Codex CLI MCP bridging — fork-side MCP server proxies
  external tool calls so non-MCP CLI models can use the same toolbox.
- `.mcp.json` loader (Claude Code format) at `internal/config/mcp_json.go`.
- Session resume across messages using prefix-hash validation, for
  Anthropic prompt caching (`1b9bd849`, `a32ce147`).
- Permission requests for external MCP tools (`a028f1cb`).
- Built-in CLI tools (WebSearch, WebFetch, Task, Agent) allowed in
  MCP mode (`f0471e77`).

### 4.F — Hooks in config

`internal/hooks/` was added upstream but we extended the `Hooks` block
in `crush.json` schema and surfaced it in `schema.json` so the user can
configure PreToolUse/PostToolUse hooks via the standard config file
(see `docs/hooks/README.md`).

### 4.G — Front-end fallbacks

- **Per-session WebSocket message filtering** — incoming `messages`
  events are filtered against the currently active `sessionID` so an
  agent running in another session does not bleed into the open chat
  (`ceb1e1a4`).
- **Yolo sync on reconnect** — the WS client re-sends its yolo flag on
  every reconnect so a backend that restarted with empty per-session
  state inherits the user's last choice (`734ecc62`).
- **Empty-response rendering** — `web/src/components/Message.tsx`
  renders a visible `FinishErrorBlock` whenever a `finish` part carries
  `Reason === "error" | "canceled"`, and shows a placeholder for
  assistant messages that finished without any visible part. This
  pairs with Section 4.D's server-side empty-response detection. Marker:
  `// Fork patch: render explicit error/empty finish parts...`

### 4.H — Misc developer features

- `Makefile`, `build.go` — convenience entry points.
- `.claude/skills/deploy.md` — skill autocomplete entry.
- `internal/cmd/web.go` — the `crush web` subcommand (the default in
  this fork).
- `internal/swagger/` — annotations-only stub kept so a future
  REST-side rebirth could autogenerate docs; the WS server is the real
  API surface.

### 4.I — Parallel-process / multi-agent concurrency

Crush in this fork is increasingly used as a **sub-agent** spawned by an
orchestrator: typically 5+ `crush run --session X --json` processes
running concurrently in the same working directory and sharing one
`.crush/` folder (the shamir-db audit workflow is the canonical case).
On top of that, each process fan-outs internally to sub-agents in
separate goroutines. The defence layers live in two clusters.

**Cluster A — landed in `cfad5391` (2026-05-17)**

- `internal/session/lock.go` + `lock_unix.go` + `lock_windows.go` —
  cross-platform per-session exclusive file lock under
  `.crush/locks/session-<id>.lock` (flock on POSIX, LockFileEx on
  Windows). `agent.Run` acquires it after the in-process
  `IsSessionBusy` check so two crush processes cannot share a session
  id. OS auto-releases on death — no stale-lock cleanup. PID stamped
  for diagnostics.
- `recoverInterruptedTurns` age-filtered (>30s) so a starting process
  cannot mark a fresh streaming message from a parallel process as
  orphan.
- `log.go` per-entry `pid=N` attribute so interleaved Windows log
  writes can be split post-hoc via `jq 'select(.pid==N)'`.
- SQLite `busy_timeout=30000` + `synchronous=NORMAL` (already present
  via `db/connect.go`).
- `envelope.Error` / `envelope.Warnings` for orchestrator feedback.
- `CRUSH_FORBID_WRITES` env to block the model from writing through
  the write/edit tool into paths the orchestrator owns.
- coordinator auto-retry on `FinishReasonError("Stream stalled")`.

**Cluster B — landed in the follow-up (this section's commit)**

Cluster A closed the loudest races but the audit (recorded in chat
history) flagged remaining HIGH-severity gaps:

- **Atomic file writes.** `tools/write.go`, `tools/edit.go`,
  `tools/multiedit.go` previously used `os.WriteFile`, which is not
  atomic — a `kill -9` / OOM mid-write left the user's file
  truncated and the DB history snapshot taken *after* the write made
  no recovery possible. All six sites now use `fsext.AtomicWriteFile`
  (write to a sibling `.tmp` file, fsync, rename). The helper lives
  at `internal/fsext/atomic.go`.
- **Session cost: read-modify-write → atomic additive.** The
  `UpdateSession` SQL was reshaped to drop the `cost` column, and a
  new `IncrementSessionCost` (`cost = cost + ?`) was added. The
  `session.Save` Go API still exists for title/tokens/summary/todos
  but does not touch cost. New `Service.IncrementCost(ctx, id, delta)`
  for the additive path. `coordinator.updateParentSessionCost` and
  `agent.updateSessionUsage` (three call sites) now route cost
  through `IncrementCost` so concurrent sub-agent fan-out cannot lose
  accrued cost via interleaved Get-modify-Save. Token fields keep
  overwrite semantics (each step's `usage` is a cumulative snapshot,
  not a delta).
- **MCP id and settings.json flock.** `cliprovider/mcpserver.go`'s
  `qwenMCPID`, `geminiMCPID`, `registerQwen/GeminiMCP`,
  `deregisterQwen/GeminiMCP` previously did read-then-write of
  `.crush/{qwen,gemini}-mcp-id` and `~/.{qwen,gemini}/settings.json`
  without locking — two parallel `crush run` startups on a fresh
  project could both miss the id file, both generate distinct UUIDs,
  and end up with a split-brain MCP server name; settings.json edits
  could clobber each other. All six now wrap the critical section in
  `session.AcquireFileLock` (blocking variant of `TryAcquireSessionLock`,
  exposed via the new `internal/session/file_lock.go` +
  `file_lock_{unix,windows}.go`) and use `fsext.AtomicWriteFile` for
  the write.
- **Log rotation actually runs.** `internal/log/log.go` raised
  `MaxBackups` from 0 (= rotation disabled) to 3 and enabled
  `Compress`. Without this the active log file would grow unbounded
  the moment MaxSize was exceeded under parallel-process write
  pressure.
- **Permission cache invalidation.** `internal/permission/permission.go`
  previously held an in-memory cache of persistent grants loaded
  exactly once at startup, with `Request` scanning it under a RWMutex.
  An "always allow" granted in process A was therefore invisible to
  process B until B restarted (and in non-interactive `crush run` B
  would block on the prompt). The cache was removed; `Request` now
  hits the DB via a new indexed `MatchSessionPermission` SQL on every
  call. `GrantPersistent` stores `session_id=""` so the row matches
  any session, preserving the cross-session contract of the old
  loader.

**Follow-up polish (from the post-batch code review):**

- `internal/cmd/sessions.go` `sessions reset` was broken by the
  Save-drops-cost contract change — it set `sess.Cost=0` then called
  Save (which now ignores cost). Fixed by tracking the previous cost
  and applying a negative `IncrementCost` delta.
- `session.AcquireFileLockContext(ctx, path)` added next to the
  indefinite-blocking `AcquireFileLock`. Implemented as polling
  `TryAcquireFileLock` with exponential backoff (25→500ms cap) so a
  cancelled ctx does not leak an orphan goroutine holding the lock.
  `cliprovider/mcpserver.go` introduces `acquireMCPConfigLock` that
  wraps it with a hard 30s timeout — all six MCP id/settings flocks
  now use it, so a wedged sibling crush process cannot freeze the
  whole parallel-run fleet.
- New migration `20260517000001_permissions_dedup_and_indexes.sql`
  adds two things: a UNIQUE index on
  `session_permissions(session_id, tool_name, action, path)` to
  support `INSERT … ON CONFLICT DO NOTHING` in CreateSessionPermission
  (prevents duplicate rows on repeated Always-Allow clicks now that
  the in-memory cache is gone), and a composite index on
  `(tool_name, action, path, enabled)` so `MatchSessionPermission`'s
  WHERE clause is index-backed under multi-process auto-approve load.
  The migration also pre-dedups any existing duplicate rows (keeping
  the oldest by id) so the UNIQUE constraint can be applied
  retroactively.
- `Service.IncrementCost` docstring on the interface now spells out
  the `delta == 0 → Get` short-circuit (used by
  `coordinator.updateParentSessionCost` to keep the parent-not-found
  error path).
- `TestDisabledPermissions_NotMatched` wraps its event receive in a
  `select` with a 2s deadline so a regression that silently
  auto-approves the disabled rule fails fast instead of waiting for
  `go test -timeout`.

**Audit items intentionally deferred (still HIGH but lower urgency):**

- The original audit listed "filetracker mtime persisted per-process"
  as deferred HIGH. A re-read of the code in this follow-up showed
  the filetracker is **already DB-backed** via the `read_files` table
  with `ON CONFLICT … DO UPDATE SET read_at = excluded.read_at`
  (`internal/db/sql/read_files.sql`). Last-writer-wins is benign for
  the cross-process case. Item resolved as a no-op — please don't
  re-open it.
- The audit's "parallel MCP stdio children" item (N processes spawn
  N children of every configured MCP server) is a design-level issue
  that needs documentation + an HTTP/SSE-transport recommendation,
  not a code fix. Tracked separately.

## 5. In-code markers

Whenever we patch an **upstream** file in a non-obvious way we leave a
comment like:

```go
// Fork patch: <one-line reason>. See CHANGELOG.fork.md (Section <X>).
```

Search the repo for `Fork patch:` to find every such site. Current
sites of interest (placed **after** the `package` line, not before, so
they don't pollute `go doc` output):

- `internal/cmd/root.go` — web subcommand replaces TUI launcher
  (Section 2 / 4.A).
- `internal/cmd/login.go` — no Go REST client to sync credentials to
  (Section 2).
- `internal/config/config.go` — Hooks block exposure + simplified
  ExtraHeaders/Body docs (Section 4.F).
- `internal/message/message.go` — added Pinned/Hidden/ReasoningEffort
  + removed upstream's debounced update layer (Section 2 / 4.C).
- `internal/permission/permission.go` — persistent per-session
  permissions + JSON tags moved to the wire layer (Section 4.A / 4.C).
- `internal/agent/coordinator.go` — N-session web orchestration,
  ModelOverride, TakeSummarizeQueue, cliprovider wiring (Section 4.D
  / 4.E).
- `internal/agent/agent.go` — empty-response detection in
  `OnStepFinish` (Section 4.D). This marker is inline at the patch
  site (not a file header) because it's a single-block change.
- `web/src/components/Message.tsx` — `FinishErrorBlock` rendering and
  empty-content fallback (Section 4.G). Two inline markers, one per
  patch site.
- `internal/agent/tools/write.go` / `edit.go` / `multiedit.go` —
  atomic file writes via `fsext.AtomicWriteFile` (write to tmp +
  rename) so a `kill -9` / OOM mid-write cannot leave the user's file
  half-truncated. Six sites total. See Section 4.I.
- `internal/db/sql/sessions.sql` — `UpdateSession` no longer writes
  the `cost` column; new `IncrementSessionCost` provides
  `cost = cost + ?` atomic accumulation. Matches
  `internal/session/session.go` `IncrementCost` service method. See
  Section 4.I.
- `internal/agent/agent.go` — `updateSessionUsage` now returns the
  cost delta; three call sites (main step-finish, summarize, manual
  summarize) call `sessions.IncrementCost` for the additive write.
  Marker is on the function comment.
- `internal/agent/coordinator.go` — `updateParentSessionCost`
  rewritten to use `IncrementCost` so concurrent sub-agent fan-out
  does not lose accrued cost via read-modify-write. See Section 4.I.
- `internal/agent/cliprovider/mcpserver.go` — `qwenMCPID`,
  `geminiMCPID`, `registerQwenMCP`, `deregisterQwenMCP`,
  `registerGeminiMCP`, `deregisterGeminiMCP` are wrapped in a
  `session.AcquireFileLock` on a sibling `.lock` file and switched to
  `fsext.AtomicWriteFile` so two parallel `crush run` processes
  cannot split-brain MCP IDs or clobber `~/.{qwen,gemini}/settings.json`.
  See Section 4.I.
- `internal/log/log.go` — lumberjack `MaxBackups` raised from 0 to 3
  and `Compress` enabled so the log file does not grow unbounded
  under parallel-process workloads. See Section 4.I.
- `internal/permission/permission.go` — startup pre-load into an
  in-memory cache was dropped; `Request` now consults the
  `MatchSessionPermission` SQL on every call so a persistent "always
  allow" granted in process A is immediately visible in process B
  without restart. `GrantPersistent` stores `session_id=""` to
  preserve the cross-session contract. See Section 4.I.

Larger fork-only files (the entire WebSocket server, `web/`,
`cliprovider` additions, etc.) do not need per-site markers — their
existence is the marker. Fork-only utility files added for the
concurrency layer (`internal/fsext/atomic.go`,
`internal/session/file_lock*.go`) are also marker-free for the same
reason.

## 6. Chronological commit log

Format: `<short-hash> <date> <subject>`. All commits authored by
`Marat K <phpcraftdream@gmail.com>`. Generated with:

```
git log --pretty=format:"%h %ai %s" --no-merges --reverse origin/main..HEAD
```

Re-run after each upstream merge and append the new commits below the
previous batch.

### Batch 1 — Initial fork (2026-03-09 → 2026-03-13)

```
7ff2292e 2026-03-09 ✨ feat(web): replace TUI with React web UI and WebSocket server
f1a406bb 2026-03-09 ✨ feat(web): session management with hash-based routing and inline rename
6c5b81b6 2026-03-09 ✨ feat(web): model catalog, provider API keys, yolo mode, and telemetry removal
93077430 2026-03-09 ✨ feat(web): message actions, CLI providers, system prompt, and permissions
87464c10 2026-03-09 ✨ feat(web): message queue, stop button, rerun, generation time, and MCP management
d57400c4 2026-03-09 ✨ feat(web): dark/light theme system and UI polish
c3ad81aa 2026-03-09 feat(web): add LSP server management UI identical to MCP settings
f60d19c7 2026-03-09 ✨ feat: add todos UI and expose todos tool to CLI providers
8ff59020 2026-03-09 ✅ test(web): add Playwright tests for LSP, providers, theme, system prompt
16187cde 2026-03-09 🐛 fix(web): remove duplicate message on rerun
00983411 2026-03-09 🐛 fix(cliprovider): block TodoWrite so model uses mcp__crush__todos
44ca7f4f 2026-03-09 feat(cliprovider): add MCP integration for Gemini and Codex CLIs
23bb5069 2026-03-09 fix(agent): inject actual todos state into system_reminder
4b634f9e 2026-03-09 feat(ui): add task creation button and move row controls to left
e3ea1276 2026-03-09 feat(debug): add detailed logging for todo save operations
a17bf6a9 2026-03-09 fix(todos): protect user edits from model overwrite with merge strategy
0afdede9 2026-03-10 fix(todos): prevent summarize from overwriting user edits
edaf20e7 2026-03-10 ♻️ refactor(todos): model list is authoritative, only protect status direction
acbe9d7f 2026-03-10 fix(todos): strengthen system_reminder to prevent restoring deleted tasks
95a71fa8 2026-03-10 feat(skills): slash command autocomplete with multi-tool discovery
a60ffc40 2026-03-10 feat(ui): branding, font system, dark mode, git info, yolo, permissions
a871135e 2026-03-10 feat(ui): fork session button on messages; instant chat scroll
3a2c828d 2026-03-10 ⚡️ perf(ui): memo, useCallback/useMemo, component decomposition
bc4008af 2026-03-10 ✨ feat(messages): add pinned messages
17fede9f 2026-03-10 ♻️ refactor(ui): semantic CSS aliases + RGB color system
ce476dbc 2026-03-10 ⚡️ perf(build): add React Compiler via babel-plugin-react-compiler
734ecc62 2026-03-10 🐛 fix(ws): sync yolo state to server on every reconnect
07311167 2026-03-10 🎨 style(ui): pinned messages always show star + yellow accent border
9eb7e5e5 2026-03-10 🐛 fix(agent): resolve stuck busy state after failed/slow summarize
b6b3d23c 2026-03-10 ⚡️ perf(build): fix React Compiler integration via @rsbuild/plugin-babel
9886d533 2026-03-10 ✨ feat(agent): always continue after auto-summarization
4ab24e0b 2026-03-10 🐛 fix(ui): always show star and highlight for pinned messages
b899cb4c 2026-03-10 ✨ feat(agent): sliding context window + queued compact
1086fe13 2026-03-10 🔧 chore(deps): add @rsbuild/plugin-babel to package-lock
107c5faa 2026-03-10 ✨ feat(agent): silent background compaction of oldest 50% of messages
7665b094 2026-03-10 ✨ feat(cliprovider): stream tool calls from CLI models via MCP bridge
da090bf7 2026-03-12 feat(ui): double scrollbar width from 8px to 16px
ceb1e1a4 2026-03-12 fix(web): filter WebSocket messages by active session ID
6a39c56e 2026-03-12 feat: add per-session YOLO mode and persistent permissions
c669a5ef 2026-03-12 feat: add permissions management modal and backend API
ad456c73 2026-03-12 test: add comprehensive UI tests for new features
70574d30 2026-03-12 Fix UI test failures and improve session-based message routing
6ede4209 2026-03-12 Add Shift+scroll for 5x faster chat navigation
db54815c 2026-03-12 Prevent text selection when Shift-clicking message checkboxes
f58e2159 2026-03-12 Add select-none to message checkbox wrapper for cleaner range selection
bc7124b7 2026-03-12 Fix SQL queries to use prepared statements instead of raw SQL
baf59ddc 2026-03-12 Fix YOLO toggle: persist to backend per-session, add E2E tests
8868dced 2026-03-12 Remove localStorage for settings, use backend as source of truth
2a9108a2 2026-03-12 Add CWD display, logs modal, and fix YOLO UI tests
e13d9dca 2026-03-12 Add data-test-id attributes to all UI components
1fcd6657 2026-03-13 Fix test selectors: standardize on data-test-id
16c9920f 2026-03-13 Add claude-sonnet-1m and claude-opus-1m CLI models
90c0c2a3 2026-03-13 Set cli-claude-opus-1m/sonnet-1m as default models for local-cli
8b3707dd 2026-03-13 Remove deprecated --effort high thinking models
38cdccb7 2026-03-13 Add reasoning effort control for Claude CLI models
```

### Batch 2 — CLI providers, attachments, sub-agents (2026-03-27 → 2026-03-30)

```
d65d53b4 2026-03-27 Rework CLI thinking models and propagate reasoning effort
59f3557d 2026-03-27 Fix session updated_at not being set on save
a26e5d79 2026-03-27 Fix FinishToolCall dropping ProviderExecuted field
c0304f6c 2026-03-27 Skip disabled permissions when loading on startup
e404f688 2026-03-27 Fix web UI: remove extra button, fix syntax, improve modals
b46dae6c 2026-03-27 Remove obsolete YOLO and permissions test files
c1f0b5bb 2026-03-28 Update Claude CLI models to 1M context window
c57d9dc7 2026-03-28 Add clipboard image paste and file attachment support for CLI agents
abdfbf1b 2026-03-28 Improve UI layout: more chat space, fix overflow, add clear tasks
bd800f62 2026-03-28 Fix typography and horizontal scroll
f2856d31 2026-03-28 Fix horizontal scroll: use overflow-wrap anywhere
45a03861 2026-03-28 Fix file attachments: increase WS limits, paste any file type
690d433f 2026-03-28 Save attachments to disk and inject paths into prompt text
8f88f410 2026-03-30 Inline sub-agent display in chat instead of separate session tabs
69b210ef 2026-03-30 Fix queued messages lost on Stop: send all as single combined message
8f57c937 2026-03-30 Show tool call names in stderr and fix multi-part text output
01151a24 2026-03-30 Add visual block separation for assistant messages based on time gaps
f087ccaf 2026-03-30 Add Claude CLI models invoked via npx @anthropic-ai/claude-code
2222c989 2026-03-30 Enable effort toggle arrows for npx Claude models
ea8e78d3 2026-03-30 Pass reasoning effort from session to CLI models via context
31592eff 2026-03-30 Fix npx Claude models: use pipe instead of PTY, add --yes flag
ab200e86 2026-03-30 Fix npx models: use NoPTY + PromptFlag instead of AlwaysStdin
cd868df2 2026-03-30 Merge stdout+stderr for NoPTY models to handle long prompts
2d35883d 2026-03-30 Force stdin for NoPTY models to avoid Windows cmd line length limit
a9897fca 2026-03-30 Fix NoPTY pipe: use StdoutPipe+StderrPipe with concurrent copy
0948af4c 2026-03-30 Load MCP servers from .mcp.json files (Claude Code format)
5b68c5e8 2026-03-30 Proxy external MCP tools to CLI models via crush MCP bridge
606dcfec 2026-03-30 Increase default timeout for .mcp.json servers to 60s
a028f1cb 2026-03-30 Add permission requests for external MCP tools in CLI bridge
1b9bd849 2026-03-30 Resume CLI sessions across messages for API prompt caching
a32ce147 2026-03-30 Add prefix hash validation for CLI session resume and cache logging
f0471e77 2026-03-30 Allow CLI built-in tools (WebSearch, WebFetch, Task, Agent) in MCP mode
```

### Batch 3 — Sub-agent guards (2026-05-01)

```
36aac56e 2026-05-01 fix: guard against undefined prompt in SubAgentBlock
21c4294a 2026-05-01 fix: guard against undefined Parts in SubAgentBlock messages
```

### Batch 4 — Upstream merge + post-merge fixes (2026-05-16, pending commit)

The merge of upstream `main` (173 commits) was first attempted as an
"evil merge" that silently dropped `internal/workspace/`. After that
merge a separate fix was applied to the empty-response handling, which
in turn motivated the writing of this document.

Open work, not yet committed:
- Re-running the `origin/main..HEAD` merge with explicit conflict
  resolutions per Section 2.
- The empty-response detection patch in `agent.go` and matching
  fallback in `Message.tsx`.
- This document.

When this batch lands, replace this paragraph with the new commit list.

### Batch 5 — Parallel-process safety, cluster B (2026-05-17)

The cluster A work landed as `cfad5391`. Cluster B (this batch) closes
the remaining HIGH-severity items from the post-`cfad5391` audit and
re-classifies one item (filetracker) as already-fixed.

Files touched (no merge from upstream pending; all our own):

```
internal/agent/tools/write.go            atomic write (fsext.AtomicWriteFile)
internal/agent/tools/edit.go             atomic write × 3
internal/agent/tools/multiedit.go        atomic write × 2
internal/fsext/atomic.go                 new helper, write-tmp + rename
internal/db/sql/sessions.sql             UpdateSession drops cost; new IncrementSessionCost
internal/db/sql/permissions.sql          new MatchSessionPermission
internal/db/{sessions,permissions}.sql.go  regenerated via sqlc
internal/session/session.go              Service.IncrementCost
internal/session/file_lock.go            generic FileLock (Acquire/TryAcquire)
internal/session/file_lock_{unix,windows}.go  blocking variants of tryLockFile
internal/agent/agent.go                  updateSessionUsage returns delta; 3 IncrementCost sites
internal/agent/coordinator.go            updateParentSessionCost → IncrementCost
internal/agent/coordinator_test.go       tests updated for IncrementCost API
internal/agent/cliprovider/mcpserver.go  flock + atomic write on qwen/gemini id + settings.json
internal/log/log.go                      lumberjack MaxBackups 3 + Compress
internal/permission/permission.go        in-memory cache dropped; DB-direct lookup
internal/permission/permission_test.go   tests updated for DB-backed flow
CHANGELOG.fork.md                        this entry + Section 4.I
```

When the commit hash is known, append a line below:

```
<hash> 2026-05-17 feat(concurrency): atomic writes, additive cost, MCP flock, permission DB lookup
```
