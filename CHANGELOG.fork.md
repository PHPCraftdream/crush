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

Upstream Crush is a **terminal UI** (TUI) coding agent — built around
a human typing into a terminal session. The optional `client/server`
mode in upstream still drives the TUI; its REST API under `/v1/...` is
just a transport between the TUI front-end (running in a separate
terminal) and the agent back-end (Unix socket / Windows named pipe).

This fork has a fundamentally different goal: **make `crush` a tool
that other AI agents can drive.** The primary user is not a human at
a keyboard — it is an orchestrator (Claude Code, a custom LLM wrapper,
a CI job, a multi-agent fleet) that spawns `crush run` and parses the
JSON envelope on stdout. A React/Tailwind web UI stays for the cases
where a human DOES want to peek in, but the design centre is the CLI
and the orchestrator-facing contract.

Concretely, this single repositioning forces everything that follows:

- The TUI is removed because the primary user no longer types.
- `crush run` grows a wrapper-stable JSON envelope, a frozen set of
  flags (`--role`, `--session`, `--format`, `--agents`, `--timeout`,
  `--json`, `--stream`), and post-validation guarantees so a script
  on top can branch deterministically on the result.
- Multiple concurrent `crush run` against one repo become a supported
  scenario (multi-section audits, fan-out refactors) — so the storage
  layer, lock files, log writes and MCP-id files all get hardened
  against process races.
- Errors are honest: when the model breaks its contract (invalid JSON,
  context overflow, mid-stream stall, sub-agent fan-out without a
  final composition) the envelope says so. The agent on top cannot
  read minds; silent success is the worst class of failure.
- Bootstrap helpers (`crush claude-init`) drop a delegation guide into
  the workspace so an upper LLM learns the rules of engagement
  without trial-and-error.

Everything we kept, removed, or rewrote — including the React web UI,
the persistent SQLite-backed sessions, the cross-process file locks,
the additive cost SQL — follows from "be a good tool for an agent on
top".

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

### Default rule — auto-reject all upstream TUI / client-server features

**Anything upstream adds that exists to serve their TUI or their
client-server REST architecture is rejected without discussion.** This
is the fork's core posture, not a per-merge judgement call. Do not stop
to ask the user about TUI/CS additions — they have made this decision
once and it is permanent. Concrete patterns:

- New `internal/ui/*` files (pills, picker, scrollbar, sidebar,
  notifications widget, mark/marker UI markers, etc.) — `git rm`.
- New `internal/backend/*`, `internal/client/*`, `internal/workspace/*`,
  `internal/cmd/server*.go`, `internal/cmd/session.go`,
  `internal/cmd/logout.go` — `git rm`.
- New REST proto types in `internal/server/proto.go` or new
  `internal/proto/*.go` files specifically wired to client-server
  (e.g. `proto/skills.go` shipping `SkillsEvent`) — `git rm`.
- New backend abstractions like `skills.Manager`, `skills.Catalog`,
  `workspace.AppWorkspace`, anything with `WithGlobalMirror` /
  cross-client RPC plumbing — `git rm` (the entire file/type).
- Auto-merged callsites of such removed types (e.g. `app.Skills` field,
  `skillsMgr` parameter, `s.applyEstimatedUsageState(...)`) — surgically
  removed from our existing files. Auto-merge will silently leave these
  in: search with `grep -rn '<removed-type-or-method>' internal/` after
  every merge and clean them up.
- New `setupEvents()` / `setupSubscriber()` style helpers that bridge
  service brokers into a `tea.Msg` pubsub for bubbletea — drop the whole
  block. Our WebSocket hub (`internal/server/hub.go`) handles fan-out
  directly without going through tea.Msg.
- TUI-driven backend fields like `Session.EstimatedUsage`,
  `service.estimatedUsageMu`/`estimatedUsage` map and their setter
  methods — drop entirely. The marker feature serves a TUI widget we do
  not ship; the WebUI computes usage display from the event stream.

If a *behavioural fix* arrives that happens to be packaged with TUI
code (e.g. a `fix(ui): ...` commit that incidentally touches a session
service), extract only the non-UI part and apply it manually — do not
take the wrapper. The upstream UI infrastructure has zero callers in
our fork's WebSocket+React surface, so any of it linked in becomes
dead-but-compiled code that drags in bubbletea/lipgloss/fang.

The narrow exception is `internal/ui/notification/` — OS-native
notifications (Windows toast, macOS NotificationCenter, Linux libnotify)
that the web server reuses to surface re-auth / out-of-credits events.

### Per-file conflict table

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
505d0a9c 2026-05-17 feat(concurrency): cluster B — atomic writes, additive cost, MCP flock, permission DB lookup
2d8c60b5 2026-05-17 feat(run): --format and --agents flags + JSON envelope stripper
63bc98cc 2026-05-17 fix(deploy): walk PATH manually so Go 1.19+ exec.ErrDot doesn't break deploy from repo root
```

### Batch 6 — orchestrator UX hardening from 2026-05-17 audit feedback

The shamir-db 2× parallel audit run produced a focused bug report
(BUG 1 .. BUG 5 + Observation 6). This batch closes all of them.

**BUG 1 (HIGH) — `--format json` could yield invalid JSON silently.**
Stripper's brace-walker accepted balanced-but-not-valid output (real
case: model forgot the closing `]` before `,"post_flight":`). Added
`stripAndValidateJSON` which runs `json.Valid` on the stripped
candidate; on failure the envelope reports
`exit_reason="invalid_json"`, restores the ORIGINAL to `final_text`,
moves the invalid candidate to `assistant_notes`, and puts a
position-bearing `json.SyntaxError` in `error`. New `ErrInvalidStripJSON`
sentinel. 6 new tests in `strip_json_test.go` including the exact
shamir-db reproducer.

**BUG 2 (MEDIUM) — model ignores "no preamble" hint.**
Tightened `formatPresetJSON` (imperative voice, 8 explicit rules,
last-line repeat). Added `StrippedBytes` field to the envelope so
operators can graph compliance over time.

**BUG 3 (LOW) — `crush models` dials home on every call.**
Added `cache.Age()` + `providerCacheTTL` (24h default, override via
`CRUSH_PROVIDER_CACHE_TTL` env). `catwalkSync` and `hyperSync` now
skip the HTTP roundtrip when the on-disk cache is fresher than the
TTL. Test pkg `TestMain` sets TTL=0 so existing network-path tests
keep exercising it.

**BUG 4 (MEDIUM) — empty `error.code` when `exit_reason=error`.**
`buildRunResult` now emits a fallback message naming the most likely
causes (provider HTTP error, stream stall, OOM, context overflow)
when the model's Finish part carried no Message/Details. Also adds
a `truncation_hint` warning when `final_text` ends with `: , -` —
hints that the model was mid-composition when the error fired.

**BUG 5 (MEDIUM) — write-tool overwrites the stdout-redirect target.**
Already mitigated in `cfad5391` (CRUSH_FORBID_WRITES env). This batch
documents it: `crush run --help` gained a "Protecting harness-owned
files" section and the claude-init guide gained a matching section
with the canonical `CRUSH_FORBID_WRITES="$out" crush run > "$out"`
pattern. Marker bumped v5 → v6.

**Observation 6 — show pricing in `crush models show`.**
`modelsShowCmd` now prints `ctx=204.8k` + `$1.40 / 1M in, $4.40 / 1M
out (cached-in $0.26)` on a second indented line per slot, sourced
from the catwalk catalog. Same fields appear under `cost_per_1m_*`
keys in `--json` output. Custom/local models without a catalog
entry silently omit pricing (no broken display).

Files touched:

```
internal/app/app.go                  fallback error msg, truncation warning, validation plumbing
internal/app/strip_json.go           stripAndValidateJSON, ErrInvalidStripJSON
internal/app/strip_json_test.go      6 new validation tests
internal/app/run_result_test.go      updated for new buildRunResult signature
internal/cmd/run.go                  CRUSH_FORBID_WRITES help section
internal/cmd/run_format.go           tightened formatPresetJSON
internal/cmd/claude_init.go          v5 → v6 marker, CRUSH_FORBID_WRITES section
internal/cmd/models_set.go           pricing + ctx in `models show`, JSON keys
internal/config/provider.go          cache.Age()
internal/config/catwalk.go           providerCacheTTL + TTL skip
internal/config/hyper.go             TTL skip mirror
internal/config/load_test.go         TestMain disables TTL for the pkg
CHANGELOG.fork.md                    this entry
```

### Batch 7 — sub-agent aggregation + invalid_json regression fix (2026-05-17, session #3 feedback)

The shamir-db session #3 audit (5 parallel agents, glm-5.1,
markdown output) surfaced two issues:

**BUG (regression I introduced in batch 6): `exit_reason="invalid_json"`
fires under `--json` even when the operator did NOT pass
`--format json`.** Root cause: `StripJSONFences` trigger included
`asJSON` (the envelope flag). But `--json` is purely an envelope
shape; an operator can legitimately want JSON envelope with
markdown final_text inside. Wiring `--json` into the JSON-validation
trigger was a false-positive trap. Fix: trigger only on
`--format json` / `--format json-schema:*`. ~2 LoC + 11 unit tests
in `internal/cmd/run_format_test.go` covering format flag flow and
`composeUserPrompt` ordering.

**HIGH: parent collapses sub-agent fan-out output 7× in extreme
cases.** When the model uses the `agent` tool and you let
`--agents agent-allow` (default) run, the parent often *summarises*
sub-agent outputs into a one-paragraph wrap-up. The orchestrator
sees only the wrap-up, the detail lives in DB sub-sessions where
the upper LLM cannot easily reach it. Three-layer response:

1. **ALWAYS-ON warning.** When parent dispatched ≥2 sub-agents and
   `final_text` is <40% of their combined character count, append
   `"reduction-loss: final_text is N chars (X% of M combined
   sub-agent chars across K sub-session(s)). Re-run with
   --aggregation=attach or --aggregation=concat to recover…"` to
   `envelope.warnings`. No opt-in — the operator sees it whether
   or not they know the flag exists.

2. **OPT-IN `--aggregation concat`.** Appends a prompt nudge
   (`aggregationConcatPromptHint`) asking the parent to include
   each sub-agent's reply verbatim in `final_text`, in dispatch
   order, with labelled headings. Best for "I want one big string
   to grep through" workflows.

3. **OPT-IN `--aggregation attach`.** After Run completes, app
   queries the session DB for every sub-session with
   `parent_session_id == this run`, fetches each sub-session's
   last finished assistant message via `Messages.List` +
   `lastAssistantText`, truncates to `maxSubAgentTextChars` per
   entry, and surfaces them as
   `envelope.sub_agent_outputs: [{session_id, title, final_text,
   char_count}]`. Parent's `final_text` becomes a brief wrap-up
   (driven by `aggregationAttachPromptHint`). Best for machine
   consumers that want the structured set.

New SQL `ListSubSessions(parentSessionID)` (in `db/sql/sessions.sql`,
indexed on `parent_session_id` already). New service method
`Session.ListSubSessions`. `mockSessionService` updated accordingly.
New helper file `internal/app/sub_agents.go` with
`collectSubAgentOutputs`, `subAgentSummaryStats`, `lastAssistantText`
+ `maxSubAgentTextChars` cap.

`composeUserPrompt` now takes 4 hint arguments
(`format → agents → aggregation`) matching reasoning order.

Files touched:

```
internal/db/sql/sessions.sql            new ListSubSessions query
internal/db/sessions.sql.go             sqlc regen
internal/session/session.go             Service.ListSubSessions + impl
internal/app/app.go                     reduction warning logic, sub_agent_outputs plumbing, RunOverrides.AggregationMode, runResult.SubAgentOutputs
internal/app/sub_agents.go              new — collector + stats + lastAssistantText
internal/app/sub_agents_test.go         new — 6 lastAssistantText cases + bound check
internal/app/run_result_test.go         4 new tests: reduction warning + sub_agent_outputs
internal/app/resolve_session_test.go    mockSessionService.ListSubSessions
internal/cmd/run.go                     --aggregation flag, invalid_json regression fix (drop asJSON from StripJSONFences trigger)
internal/cmd/run_format.go              composeUserPrompt 4-arg, aggregationConcatPromptHint, aggregationAttachPromptHint
internal/cmd/run_format_test.go         new — 16 tests for resolveFormatHint, composeUserPrompt, hint constants
internal/cmd/claude_init.go             v6 → v7 marker, new "Sub-agent aggregation" section
README.md                               --aggregation flag, sub_agent_outputs envelope field, reduction-loss warning
CHANGELOG.fork.md                       this entry
```

### Batch 8a — Multi-JSON extractor (side patch, 2026-05-17)

Small models (GLM-5-turbo etc.) often emit prose preamble + JSON or even
multiple JSON values separated by prose. The new `stripAndExtractJSON`
replaces `stripAndValidateJSON` as the runtime entry point: it scans for
ALL balanced JSON values, validates each, and wraps N≥2 into a JSON array
with forensic notes. The old single-shot function is retained for existing
tests.

```
internal/app/strip_json.go             findAllBalancedJSONValues, stripAndExtractJSON, buildStripNotes
internal/app/strip_json_test.go        6 new TestStripAndExtractJSON_* tests
internal/app/app.go                    route RunNonInteractive through stripAndExtractJSON
CHANGELOG.fork.md                      this entry
```

### Batch 8 — Survive SIGTERM during final composition (2026-05-17)

Four coordinated changes to prevent text loss when the process is killed
during the final streaming phase:

**1. Auto-checkpoint: periodic DB flush of in-progress assistant text.**
A coalescing ticker goroutine inside `Run()` periodically clones the
in-memory assistant message, adds `Finish{Partial: true}`, and calls
`messages.Update` under `sessionLock`. The ticker only writes when
`Parts` have actually changed (coalescing). The `Partial` finish marker
does NOT set `finished_at` in DB and does NOT count as "finished" for
`IsFinished()`. Default interval: 2s. Configurable via
`Options.CheckpointIntervalSeconds` (>0 = that many seconds, -1 =
explicitly disabled, 0 = use default). The goroutine is started on first
`OnTextDelta` and stopped on `OnStepFinish`.

**2. Smart timeout: `--timeout-extends-on-progress` resets deadline on
streaming activity.** When enabled, the stream watchdog extends its
deadline every time `bump()` is called (which happens on every streaming
event). An absolute hard cap from start time is enforced via
`--timeout-hard-cap` (0 = no cap). Without the flag, original idle-only
behavior is preserved. Rate-limited INFO log for deadline extensions
(every 30s). Flags are plumbed through `RunOverrides` →
`Coordinator.SetAgentTimeoutOptions` → `SessionAgent.SetTimeoutOptions`.

**3. Final composition log: INFO log when agent enters final text phase
after tool boundaries.** A `sawToolBoundary` bool is set on every tool
callback and reset after the first `OnTextDelta` that follows. When a
text delta arrives after tool boundaries, a single `slog.Info` is
emitted per step — a forensic marker for post-mortem debugging of
"time from tool-use to final-text-started".

**4. Recovery: `findOrphanPartial` surfaces orphan text in `--json`
envelope.** Scans session messages backwards for the latest assistant
with `IsPartial() && !IsFinished()`. Returns a `recoveredPartial`
struct (`{message_id, chars, last_flush_at, text}`). When found, the
JSON envelope gets `recovered_partial` populated and a WARN-level
warning appended to `warnings[]`. Only the latest orphan is surfaced.

Files touched:

```
internal/agent/agent.go                checkpoint ticker, final composition log, SetTimeoutOptions method
internal/agent/agent_test.go           TestSetTimeoutOptions smoke test
internal/agent/stream_watchdog.go      extendsOnProgress + hardCap deadline extension
internal/agent/stream_watchdog_test.go 3 new ExtendsOnProgress tests
internal/agent/checkpoint_test.go      new — 4 tests for Piece 1 checkpoint logic
internal/agent/final_composition_test.go new — 3 tests for Piece 3 logging
internal/agent/coordinator.go          wire checkpoint interval from config, SetAgentTimeoutOptions
internal/agent/coordinator_test.go     mock SessionAgent.SetTimeoutOptions
internal/message/content.go            Partial finish marker, IsPartial(), IsFinished() update
internal/message/message.go            finished_at stays NULL for partial
internal/config/config.go              CheckpointIntervalSeconds option
internal/app/app.go                    runResult.RecoveredPartial, findOrphanPartial, RunOverrides wiring, SetAgentTimeoutOptions call
internal/app/recovery_test.go          3 new TestFindOrphanPartial_* tests
internal/cmd/run.go                    --timeout-extends-on-progress, --timeout-hard-cap flags
CHANGELOG.fork.md                      this entry
README.md                              flag table, envelope fields
internal/cmd/claude_init.go            v7 → v8 marker, timeout/checkpoint/recovery section
```

### Batch 9 — claude-init always-replace, claude-del inverse, Markdown-first guideline (2026-05-17)

Three coordinated changes:

**1. `claude-init` always replaces.** Removed `--force` and `--replace`
flags. Every invocation now strips ALL existing crush-claude-init blocks
(any version) and writes a single fresh one. The slash command file
`.claude/commands/crush.md` is overwritten if it carries our sentinel; a
warning is printed if the file exists without the sentinel (someone else
owns it).

**2. `crush claude-del` (inverse).** New subcommand that undoes
`claude-init`: strips all crush-claude-init blocks from CLAUDE.md
(deletes the file if only our block was present), removes the slash
command if it has our sentinel. Idempotent and safe.

**3. Markdown-first output guideline.** Added "Output format: Markdown
is usually fine" section to both the repo's own `CLAUDE.md` and the
embedded `claude-init` template. Key idea: agents should default to
Markdown; JSON only when a CI step, jq pipeline, or fan-out harness is
the immediate consumer. Marker bumped v8 → v9.

Files touched:

```
internal/cmd/claude_init.go            rewrite: remove --force/--replace, always replace; v9 marker + Markdown section
internal/cmd/claude_del.go             new — inverse subcommand
internal/cmd/claude_init_test.go       rewrite tests for new behaviour + 6 new claude-del tests
CLAUDE.md                              Markdown-first guideline section
README.md                              update bootstrap section (remove --force/--replace, add claude-del)
CHANGELOG.fork.md                      this entry
```

### Batch 10 — Claude Haiku CLI model variants (2026-05-17)

Add four `CLISpec` entries for Claude Haiku (`--model haiku`) to the
`cliprovider.All` slice: `cli-claude-haiku`, `cli-claude-haiku-thinking`,
`cli-npx-claude-haiku`, `cli-npx-claude-haiku-thinking`. ContextWindow set
to 200_000 (Haiku 4.5 does not support 1M). Sonnet/Opus entries kept at
1_000_000 — Opus 4.6+ supports 1M, confirmed via Anthropic docs. New
test `TestAll_HaikuModelsRegistered` validates presence and context
window for all four IDs.

Files touched:

```
internal/agent/cliprovider/provider.go       4 new CLISpec entries in All
internal/agent/cliprovider/provider_test.go   TestAll_HaikuModelsRegistered
CHANGELOG.fork.md                             this entry
README.md                                     mention Haiku in CLI provider table
```

### Batch 11 — `crush models use/list/state`; `preset` and `set` removed (2026-05-17)

Reworked the model-selection UX around a small **atom registry** plus three
sharp commands. The previous two-pronged surface (`crush models set
--large X --small Y` plus the `crush models preset save/use/list/delete`
machinery) is gone — both are replaced by a single positional command and
a discoverability pair.

**1. `crush models use <large> <small>` [--global | --local].**
Two positional atoms, no flags for the model names themselves. Each
argument is either an atom from the registry (e.g. `opus-high`,
`glm5_turbo`) or a raw `provider/model[@level]` string as fallback for
anything not in the registry. `--global` (default) writes to the global
crush.json; `--local` to `./.crush/crush.json`.

**2. `crush models list`** prints the atom registry (filtered by
`EnabledProviders()` — disabled providers' atoms are hidden) followed by
an "OTHER MODELS" block listing every model id from every enabled
provider as raw `provider/model`. `--json` emits a structured object
`{atoms: [...], other_models: [...]}`. The atom rows render every effort
variant comma-separated on one line per model — opus-low through
opus-max all on the same row — so the operator can copy the exact string
they want.

**3. `crush models state` [--json]** shows the EFFECTIVE large/small
pair plus a per-scope breakdown (global / local) annotated with
`(effective)` / `(overridden by local)` / `(not set)`. When the
effective model matches an atom, the atom name is shown in parens:
`local-cli/cli-claude-opus effort=high (atom: opus-high)`. Aliased as
`crush models show` for backwards compat.

**Atom registry** (internal/cmd/models_atoms.go): 3 Anthropic atoms
(`opus`, `sonnet`, `haiku`) × 5 effort levels read from `claude --help`
at first use (cached per process, fallback `[low, medium, high, xhigh,
max]`) plus 10 Z.AI atoms with no effort. Mixed pairs supported
(`opus-high glm5_turbo`).

**Removals:**
- `crush models set` → hidden cobra with `DisableFlagParsing` that prints
  a redirect notice to stderr and exits 2.
- `crush models preset` (entire `save`/`use`/`list`/`delete` ветвь) → same
  hidden-redirect treatment. The `ModelPresets` field in crush.json is
  silently ignored from now on.

Files touched:

```
internal/cmd/models_atoms.go        new — registry, parseAtom, parseAtomOrRaw, renderAtomsBlock
internal/cmd/models_effort.go       new — claude --help parser, per-binary cache, test seam
internal/cmd/models_use.go          new — `crush models use <large> <small>`
internal/cmd/models_list.go         new — `crush models list` + --json
internal/cmd/models_state.go        new — `crush models state` + --json (aliased as `show`)
internal/cmd/models_set.go          rewrite — splitModelEffort helper + hidden redirect
internal/cmd/models_preset.go       rewrite — hidden redirect only
internal/cmd/models_atoms_test.go   new — 10 tests covering parser + lookup
internal/config/store.go            new ReadModelsAtScope helper for per-scope visibility
CHANGELOG.fork.md                   this entry
```

### Batch 13 — `crush models unset` (2026-05-17)

Adds a one-liner way to remove a model override from the chosen scope
so the other scope takes effect, replacing the `rm .crush/crush.json`
workaround (which would destroy any unrelated settings in that file).

```
crush models unset [large|small|both] [--global | --local]
```

- Defaults to `both` slots and global scope.
- Missing keys are a no-op (exit 0) with a friendly note.
- After deletion, an empty `models: {}` object is stripped so the file
  stays clean.

Files touched:

```
internal/cmd/models_unset.go        new — unset command
internal/cmd/models_unset_test.go   new — positional + registration tests
README.md                            mention unset in the models section
CHANGELOG.fork.md                    this entry
```

### Batch 14 — `crush run` auto-bypasses inner CLI permissions (2026-05-18)

Fixes the silent-hang bug where `crush run` spawns an inner CLI sub-process
(`claude`, `codex`, or `gemini`) and the inner process blocks waiting for an
interactive permission prompt that nobody is there to answer.

Mechanism: `RunNonInteractive` now seeds the agent context with
`cliprovider.NonInteractiveContextKey = true`. Inside `cliModel.Stream`,
this key short-circuits to `yolo = true` even when the caller did NOT pass
the root `--yolo` flag. Consequence: `claudeArgs(yolo=true)` adds
`--dangerously-skip-permissions`, `codexArgs(yolo=true)` adds
`--approval-mode yolo`, etc. — all transparently.

Rationale: `crush run` IS the non-interactive entry point by design. Asking
operators to remember `crush --yolo run` is fragile (CLAUDE.md actively
discouraged it). The fix makes the right thing happen automatically while
preserving the explicit `--yolo` semantics for interactive TUI/web sessions
which still go through the original `yoloFn` path.

Files touched:

```
internal/agent/cliprovider/provider.go   new NonInteractiveContextKey + override in Stream
internal/app/app.go                       seed context with NonInteractiveContextKey in RunNonInteractive
CHANGELOG.fork.md                         this entry
```

### Batch 12 — provider management commands (2026-05-18)

Extends the `crush providers` command family with full CRUD + model lifecycle
management, enabling operators to add custom providers, refresh model lists,
and track provider state transitions.

New commands:
- `crush providers list` — extended with `STATUS` column, `--grep` filtering
- `crush providers enable <id>` — re-enable a disabled provider
- `crush providers disable <id>` — disable with orphaned-slot warnings
- `crush providers add <id>` — new provider with model auto-fetch
- `crush providers remove <id>` — delete provider with `--yes` confirmation
- `crush providers update [<id> | --all]` — refresh model lists, show diffs
- `crush providers grep <pattern>` — sugar for `list --grep`

Model-source strategies:
- **Catwalk-known providers** (openai, anthropic, gemini, etc.) — pull cached
  model list from internal/config catwalk JSON; no HTTP call.
- **OpenAI-compatible (`openai-compat`)** — GET `<base_url>/models`, parse
  `{data: [{id, ...}]}`, map to catwalk.Model. Context window unknown; warns
  user to correct via future `providers update --set-ctx` flag.
- **Anthropic with custom base** — GET `<base_url>/v1/models` with
  x-api-key + anthropic-version headers; extracts id, display_name,
  context_tokens.
- **CLI providers** — model list from hardcoded cliprovider.go slice; no-op
  for update.

Orphan detection: when a provider's models change, warns if currently-preferred
`large`/`small` model is removed (`WARN: preferred <slot> = <id>/<model> no
longer exists — your '<slot>' slot is broken`).

Files touched:

```
internal/cmd/providers.go              extended: enable/disable/add/remove/update/grep + list STATUS column + --grep flag
internal/cmd/providers_test.go         new — test stubs + helper function tests (maskKey, dash, matchesGrep, providerListItem)
CHANGELOG.fork.md                      this entry
```

### Batch 15 — strip our delegation block from CLAUDE.md in sub-agent Read (2026-05-18)

Closes the silent infinite-recursion bug where a sub-agent invoked via
`crush run` would read the workspace `CLAUDE.md`, see the "delegate work
to `crush run`" guidance that `crush claude-init` injected, and faithfully
spawn ANOTHER `crush run` — recursing until the timeout fired with no
real output.

Mechanism: the MCP `Read` tool exposed by `cliprovider` (which is the
only file-read interface a CLI sub-agent has when running through us)
checks if the requested path's basename is `CLAUDE.md` (case-insensitive).
If yes, it runs the file content through the same regex
`<!-- crush-claude-init:vN -->...<!-- /crush-claude-init -->` that
`internal/cmd/claude_init.go` uses to identify the block, and strips it
before returning to the sub-agent. The file on disk is never touched —
operators and external tools see the original CLAUDE.md unchanged. Only
the in-flight Read result is filtered.

Safe-failure mode: a malformed CLAUDE.md with an opening marker but no
closing marker is returned unchanged (the regex requires both halves),
so a sub-agent gets a corrupted file's contents visibly rather than
having half the file silently disappear.

Files touched:

```
internal/agent/cliprovider/mcpserver.go               filter wired into Read handler + helpers
internal/agent/cliprovider/mcpserver_claudemd_test.go 11 tests: path detection + 5 strip scenarios
CHANGELOG.fork.md                                     this entry
```

### Batch 16 — agentguard: refuse nested AI-agent invocations from bash tool (2026-05-18)

Hard wall around the recursion path that batch 15 only patched via the
Read tool. A sub-agent that gets clever and tries to spawn another agent
through bash (the obvious workaround once CLAUDE.md no longer tells it to
delegate) now gets a tool-failure response with a clear message.

New package `internal/agent/agentguard` exports `Check(command string) error`
that returns a `*DeniedError` when the command would launch any known AI
coding agent CLI. The check is wired into BOTH bash interfaces:

  - `internal/agent/tools/bash.go` (native bash tool — used by crush
    when it runs an agent without going through a CLI provider)
  - `internal/agent/cliprovider/mcpserver.go` registerBashTool
    (the MCP Bash tool used by claude / codex / gemini when they run
    under `crush run` with --bare-style invocation)

Denylist covers the 2026 agent landscape:

  - Proprietary: claude, codex, gemini, qwen, cody, windsurf
  - Open-source: opencode, aider, cline, cursor-agent, continue, amp,
    goose, mentat, forge, tabby
  - Self: crush (recursive crush invocations are never the right answer)

Plus launch-form coverage:

  - Direct binary with extensions: .exe / .cmd / .bat / .ps1 / .sh / .py
  - Absolute and relative paths (`/usr/bin/claude`, `./claude`)
  - Shell wrappers: bash/sh/dash/zsh/ksh/fish/nu `-c "X"`, cmd `/c X`,
    powershell/pwsh `-Command X` / `-EncodedCommand X` — recursed into
  - Package runners: npx, pnpm dlx, yarn dlx, bunx, bun x, pipx run,
    uv tool run, uvx — checked against deniedNpmPackages / deniedPypiPackages
  - Wrappers: exec, command, time, nohup — stripped before matching
  - Leading env-var assignments (POSIX `FOO=bar BAR=baz claude`) skipped
    before the executable token is read
  - Chained commands: split on top-level `&&`, `||`, `;`, `|`; every
    segment is independently checked
  - Quote-aware tokenization so `claude -p "hello world"` is still
    recognised as starting with `claude`

False-positive risk on `continue` (shell keyword) and `command` (shell
builtin) is accepted — typical scripts do not invoke them as the first
token of a standalone command. If it surfaces we'll add an allowlist.

Files touched:

```
internal/agent/agentguard/agentguard.go         new — Check + denylist + tokeniser
internal/agent/agentguard/agentguard_test.go    new — 11 scenarios across launch forms
internal/agent/tools/bash.go                    early Check; deny via NewTextErrorResponse
internal/agent/cliprovider/mcpserver.go         early Check in registerBashTool; deny via toolError
CHANGELOG.fork.md                                this entry
```

### Batch 17 — standardize stdin prompt file location to `.crush/stdin/` (2026-05-18)

Documentation-only change. Formalizes a convention: when an orchestrator
(Claude Code, crush wrapper harness, CI job) needs to pass a multi-line prompt
to `crush run` via stdin (e.g. `< ./.crush/stdin/task.prompt`), it should write
to a file under `./.crush/stdin/` (co-located with workspace data) rather than
scattering files into `/tmp`. No new directories are created at runtime — the
operator's harness creates them on first write. The directory is covered by the
existing `.crush/` gitignore rule, so cleanup is automatic.

Files touched:

```
internal/cmd/claude_init.go               v9 → v10 marker; new paragraph on `.crush/stdin/` location + reuse pattern
CLAUDE.md                                 one-liner in "Long prompts" section mentioning `.crush/stdin/`
CHANGELOG.fork.md                         this entry
```

### Batch 18 — `crush sessions` subcommands for orchestrator observability (2026-05-18)

Three new `crush sessions` subcommands for monitoring and debugging session state
at the CLI. Designed for orchestrator visibility into running or completed
sessions — allowing scripts and CI jobs to inspect session metadata, monitor
lock files, and stream messages as they complete.

Subcommands:

- `crush sessions show <id> [--json] [--with-messages] [--full]` — inspect single
  session with optional message thread; default: text output with truncated
  system prompt and message previews; --json for structured format
- `crush sessions locks [--json] [--stale-only]` — scan `.crush/locks/` for
  active lock files (one per running session); report session id, PID, acquisition
  time, duration, stale status (process dead OR lock older than 10 minutes);
  default: tabwriter table; --json for NDJSON
- `crush sessions tail <id> [--follow] [--from-message <id>] [--format text|ndjson]`
  — stream messages from a session; default: print all existing messages and exit;
  --follow: poll and print new messages until session finishes or Ctrl+C;
  --from-message: resume after a specific message ID (skip earlier); default:
  human-readable blocks per message; --format ndjson: JSON per line for piping

All three commands use simple polling against the SQLite message store (no
streaming DB connections required). Error codes are predictable (0 = success,
1 = session/file not found, 2 = database error mid-stream).

Files touched:

```
internal/cmd/sessions.go                  add 3 commands + init() registration + implementation
internal/cmd/sessions_show_test.go        new — 4 tests (TextOutput, JSON, WithMessages, NotFound)
internal/cmd/sessions_locks_test.go       new — 3 tests (CreateLockFile, MultipleFiles, ParseFilename)
internal/cmd/sessions_tail_test.go        new — 3 tests (StreamsMessages, MultipleMessages, EmptySession)
CHANGELOG.fork.md                         this entry
```

### Batch 19 — `crush mcp` command family for MCP server management (2026-05-18)

Add new CLI command family `crush mcp <subcommand>` to manage Model Context
Protocol servers configured in crush.json. Follows the `crush providers`
pattern with list, show, enable, disable, restart, test, add, remove, and
set subcommands. Default write scope is --local (project config); --global
targets the user-level config.

Subcommands:

- `crush mcp list [--json] [--grep <pattern>]` — table with ID, NAME, TYPE,
  STATUS, TOOLS, COMMAND/URL columns. TOOLS shows count from live session
  or "-" if not reachable. --grep filters by id/type/command/url.
- `crush mcp show <id> [--json]` — full config for one server
- `crush mcp enable <id> [--global|--local]` — set disabled=false (default: --local)
- `crush mcp disable <id> [--global|--local]` — set disabled=true (default: --local)
- `crush mcp restart <id>` — placeholder (hot-reload planned for future)
- `crush mcp test <id> [--timeout 10s]` — connectivity test (placeholder)
- `crush mcp add <id> --type <stdio|sse|http> [--command ...] [--url ...]
  [--arg ...] [--env ...] [--header ...]` — create new server (default: --local)
- `crush mcp remove <id> [--global|--local]` (alias: `rm`) — delete from config
- `crush mcp set <id> [--command ...] [--type ...] [--arg ...] [--env ...]
  [--header ...] [--url ...] [--disabled ...]` — update fields in-place

Files touched:

```
internal/cmd/mcp.go              new — 9 subcommands + helper types + flag registration
internal/cmd/mcp_test.go         new — 15 tests covering command structure + helper functions
CHANGELOG.fork.md                this entry
```

### Batch 20 — keep MCP bridge active in yolo mode (security fix) (2026-05-18)

Closes a regression introduced by batch 14. The condition that decided
whether to spawn crush's MCP bridge inside the inner `claude` CLI was
`UseCrushMCP && !yolo && perms != nil` — the `!yolo` half meant that any
non-interactive `crush run` (which batch 14 marks as yolo-equivalent so
inner claude gets `--dangerously-skip-permissions`) silently skipped the
MCP setup. Without MCP setup we never passed `--allowedTools`, so claude
ran with its native Bash/Write/Edit/Task toolset — completely bypassing
the agentguard denylist (batch 16, which only inspects calls coming
through `mcp__crush__Bash`).

Fix: drop `!yolo` from the condition. yolo only ever needed to flip the
bypass-permissions flag for claude itself; it has nothing to say about
whether our MCP bridge sits in the loop. With the bridge on, claude's
toolset is restricted to `mcp__crush__*` plus the safe built-ins
(WebSearch, WebFetch, Task, Agent), agentguard is back in force, and
the audit/permission machinery applies to every shell call.

Verified empirically: before the patch, `crush run` invocations showed
`args: --model haiku ... --effort medium --dangerously-skip-permissions`
in cliprovider logs (no `--allowedTools`, no MCP). After: full
`--allowedTools mcp__crush__Bash,…` + `--mcp-config` lines, matching the
shape we already saw on the shamir-db workspace.

Files touched:

```
internal/agent/cliprovider/provider.go   one-line condition change at the MCP-bridge gate
CHANGELOG.fork.md                         this entry
```

### Batch 21 — `crush ping` and `crush ping-fast` (model connectivity check) (2026-05-18)

Add two new top-level commands to verify the configured large/small models
are reachable, responsive, and have valid credentials. Complements
`crush providers test <id>` (which probes the provider's API catalog endpoint);
ping is slot-based, not provider-based, and actually exercises the model
with a real completion request.

**Design choice: two separate commands vs one `--fast` flag:**
Chose two commands (`ping` and `ping-fast`) for clarity — each has its own
cobra.Command with separate help text and examples. Less cognitive load than
a flag that changes which model is tested.

**Implementation notes:**

- Both commands bypass the full agent loop and session persistence.
- Provider is built directly using the provider factories from coordinator.go.
- System prompt instructs model to reply with exactly "OK".
- Measures wall-clock latency from request start to stream completion.
- Exit codes: 0 (ok), 1 (error/auth), 2 (timeout), 3 (degraded—model responded but not "OK").
- `--json` outputs structured result with provider/model/status/latency_ms/response/tokens.
- `--timeout` defaults to 15s, `--prompt` allows custom user message.
- Cost calculation skipped (requires model pricing config, not exposed to commands).
- No session DB pollution: throwaway requests don't persist to crush sessions list.

**Tested coverage:**

- Command metadata (Use, Short, Long, Examples)
- Flag registration (json, timeout, prompt)
- JSON marshaling/unmarshaling of PingResult
- Error/timeout/degraded response handling
- Status codes mapping

Files touched:

```
internal/cmd/ping.go             new — runPing, buildPingProvider, provider builders for all types
internal/cmd/ping_test.go        new — 12 tests covering command structure + JSON output + status logic
CHANGELOG.fork.md                this entry
```

### Batch 21 — `crush ping` / `crush ping-fast` liveness probes (2026-05-18)

Cheap one-shot probes for verifying the configured large/small model is
reachable, the API key still works, and the round-trip latency. Today
operators had to run a real `crush run` with a stub prompt to test this,
which creates a session, persists messages, drags MCP setup with it,
and accumulates cost.

New commands:
- `crush ping [--json] [--timeout 15s] [--prompt "<custom>"]` — pings
  the **large** slot. Sends a one-line system prompt ("reply with OK")
  and a one-token user prompt, captures provider + model + latency_ms +
  cost_usd + tokens, returns exit 0 (ok), 1 (error), 2 (timeout), or
  3 (degraded — alive but didn't return "OK").
- `crush ping-fast` — same shape for the **small** slot.

Default text output prints provider, status, latency, response, tokens,
cost — the operator's triage info in five lines. `--json` mirrors the
shape for jq pipelines: `{provider, model, effort, atom, status,
latency_ms, response, prompt_tokens, completion_tokens, cost_usd,
error}`. Four tests in ping_test.go cover success, error propagation,
timeout, and the degraded path.

**Known limitation:** CLI-backed providers (cli-claude-*, cli-codex-*,
cli-gemini-*) currently return `Provider type not supported: "cli"` —
sub-agent skipped that branch. Fix is a one-screen patch: invoke the
underlying binary directly (e.g. `claude --model haiku -p ping`) with
a short timeout instead of going through the agent stream loop.
Deferred to a follow-up batch.

**Completion (2026-05-18):**

All implementation gaps closed:
1. Fixed `fantasy.AgentStreamCall` usage: removed message history, pass
   only `Prompt` field for stateless ping request.
2. Cost calculation implemented: looks up model pricing from
   `ProviderConfig.Models` and applies formula `(in_price/1M * prompt_tokens)
   + (out_price/1M * completion_tokens)`. Returns 0.0 if model not in
   pricing table.
3. Comprehensive test suite with 14+ functional tests:
   - `TestPingLargeModelSucceeds` / `TestPingSmallModelSucceeds` — verify
     "OK" response triggers status=ok, exit code 0
   - `TestPingErrorPropagates` — auth errors trigger status=error, exit
     code 1
   - `TestPingTimeoutDetected` — deadline exceeded triggers status=timeout,
     exit code 2
   - `TestPingDegradedResponse` — non-"OK" response triggers status=degraded,
     exit code 3
   - `TestCalculatePingCost_*` — cost calculation with various pricing
     scenarios (with pricing, no models, model not found, zero pricing)
   - `TestLookupAtomForModel_KnownModels` — atom label mapping verified
   - `TestPingResult_JSONMarshal` / `TestPingResult_WithError` — JSON
     serialization
   - Command metadata and flag registration tests

Files touched:

```
internal/cmd/ping.go         — 499 LoC, fully implemented with all provider types
internal/cmd/ping_test.go    — 582 LoC, 14 comprehensive test scenarios
CHANGELOG.fork.md            this entry
```

Also folded in this commit (background sub-agent from batch 19 kept
polishing after the initial batch-19 commit landed):
- internal/cmd/mcp.go: +158/-43 (mcpmanager integration, default
  scope changed to --local for writes, improved Long/Example help,
  better tools-list probe error handling)
- internal/cmd/mcp_test.go: +315/-69 (expanded coverage of enable/
  disable/restart edge cases, JSON output shape assertions)

### Batch 22 — remove CLAUDE.md delegation block (recursion-prone footgun) (2026-05-18)

**Postmortem.** Over the course of one day this fork accumulated 470+
orphan processes (95 claude.exe, 290 bash.exe, 80 go.exe, 3 crush.exe)
on the operator's Windows machine. Root cause: the always-on
delegation block that `claude-init` installed into the workspace's
CLAUDE.md ("you are the strategist, crush is the worker — delegate
everything"). Every Claude Code session opening the workspace read it
on startup and tried to delegate ANY task back into `crush run`, which
spawned another Claude Code via cliprovider, which read the same block,
and so on. Watchdogs killed outer parents but on Windows the child
processes became OS-level orphans (no Job Object kill-on-close).

The cycle was already partially closed at the tool-call layer:
- batch 16 (agentguard) denies `crush`/`claude`/`codex`/etc in our MCP
  Bash tool.
- batch 20 force-keeps the MCP bridge active in yolo mode so inner
  claude is locked to mcp__crush__* and agentguard actually fires.

But the cleanest fix is to remove the trigger entirely: stop telling
sub-agents to delegate.

**Change.** `claude-init` now:
- Writes NOTHING into CLAUDE.md.
- STRIPS any pre-existing crush-claude-init block (any version, v1..v10)
  from CLAUDE.md when invoked, so users upgrading from an older crush
  get a clean workspace.
- Removes CLAUDE.md entirely if stripping leaves it empty (mirrors
  `claude-del`'s behaviour).
- Continues to install / refresh the `.claude/commands/crush.md`
  slash-command — that file is now self-contained (full delegation
  guidance inline) and triggered ONLY by an explicit `/crush <task>`
  from the operator. Never auto-discovered.

Also removed:
- `crush claude-print` — there's no longer a block to print to stdout.
- The whole `claudeInitBlock()` template function in claude_init.go
  (lines 165..532, ~370 LoC of long-form prose), the
  `claudeInitMarkerStart`/`End` constants, and the related "v8"/"v9"/
  "v10" marker bump apparatus.

Test coverage rewritten for the new behaviour: 8 new claude-init tests
covering strip-only mode, empty-file deletion, multi-version stripping,
slash-command install/skip/overwrite. claude-del tests unchanged.

Defence-in-depth still in place: agentguard (batch 16) + MCP-bridge
re-activation (batch 20) keep blocking nested AI-agent invocations
even if some operator manually puts a delegation hint back. The
removed block was only the *trigger* — the agent containment layer
stays.

Files touched:

```
internal/cmd/claude_init.go        rewrite — 534 → 184 LoC
internal/cmd/claude_init_test.go   rewrite — new tests for the new behaviour
internal/cmd/claude_print.go       deleted — nothing left to print
README.md                          rewrite of the claude-init section
CHANGELOG.fork.md                  this entry
```

### Batch 23 — upstream merge v0.72.0 + system-prompt compression (2026-05-26)

This batch covers two unrelated chunks landed back-to-back: a compression
of the system prompt that ships with every `crush run`, and the
upstream-merge of 42 commits (v0.71.0 + v0.72.0).

**System-prompt compression (commit `882ffccd`)**

`internal/agent/templates/coder.md.tpl` was 4922 bytes / 96 lines —
fourteen dispersed rules plus four prose blocks (`<workflow>`,
`<editing>`, `<coding>`, `<skills_usage>`) that mostly restated the
rules in different words. Restructured into 5 thematic blocks (editing
discipline, execution, I/O contract, safety boundary, project context)
totalling 2772 bytes / 41 lines. Every behavioural hook preserved:
the will-to-finish (autonomous + finish-every-part + only-stop-for-real-
blocks + try-different-approach), the code-caution (read-before-edit +
exact-whitespace + 3–5-lines-context + strict-scope + never-commit/push/
amend/--no-verify), and the output contract (under 4 lines, no emojis,
user's language, `crush run --json` needs `final_text`). ~44 % size
reduction per invocation; saves ~600 input tokens on every native API
call and every cliprovider subprocess invocation (both render this
template).

**Upstream merge — `origin/main` 42 commits → fork main**

Backup branch: `backup/before-merge-20260526-124533`.

Per-file decisions (anything not listed below was a clean auto-merge):

| File / area | Decision | Reasoning |
| ----------- | -------- | --------- |
| All `internal/ui/*` modifications (~12 commits) | `git rm` | TUI removed in fork, see Section 2 default rule |
| `internal/server/proto.go`, `config.go`, `cmd/server*.go`, `client/`, `backend/`, `workspace/`, `cla-signatures.json`, new upstream `backend_test.go` / `ui/*_test.go` | `git rm` | client-server REST architecture not used |
| `go.mod` / `go.sum` (UU) | `--theirs` + `go mod tidy` + explicit `go get charm.land/fang/v2` + `replace github.com/u-root/u-root => v0.14.1-...` | upstream fantasy bump pulled u-root v0.16.0 which broke `mvdan.cc/sh/moreinterp/coreutils` (`gzip.New()` signature mismatch); pinned v0.14.1 to match what works with sh/moreinterp |
| `internal/agent/templates/coder.md.tpl` (UU vs upstream `1811bec2`) | combine | kept our compressed 5-block version (from `882ffccd`), wove their "use `offset`/`limit` for large files" guidance into block 1 |
| `internal/agent/tools/view.go` `DefaultReadLimit` (auto-merged to upstream 200) | manual override to **500** | upstream `1811bec2` cut 2000→200 to push agents toward offset/limit. We picked 500 as a compromise — small enough to discourage "read everything", large enough to cover most of our `.go` files in one pass so cliprovider subprocess agents (claude/codex/gemini) don't round-trip per read. `// Fork merge note` left in `view.go` |
| `internal/agent/tools/bash.go` + `safe.go` + `safe_test.go` + new `recordingPermissionService` tests (`96728b15`) | upstream verbatim | clean security fix: `containsCommandChaining()` blocks the allowlist bypass `ls && rm -rf /`. Stubbed three extra DB-backed methods on the test mock to satisfy our extended `permission.Service` interface |
| `internal/permission/permission.go` (UU + `6b312bee`) | combine | kept our DB-persistence (`ListSessionPermissions` / `UpdatePermissionEnabled` / `DeletePermission` + per-session yolo + cross-process direct-DB lookup) + adopted upstream's `skip atomic.Bool` race fix (`SetSkipRequests`/`SkipRequests` Load/Store, init via `svc.skip.Store(skip)` after struct ctor) |
| `internal/permission/permission_test.go` `TestSkipRace` | adapted | rewrote the `NewPermissionService(...)` call to our extended signature (ctx, workingDir, skip, allowedTools, q) using the existing `newTestService` helper |
| `internal/agent/agent.go` (5 UU clusters) | combine | kept our `updateSessionUsage(...) float64` signature — critical for our `sessions.IncrementCost(delta)` race-safe additive UPDATE under parallel `crush run` processes. Inside, replaced our direct token-counter assignment with upstream's `updateSessionTokenCounters` helper (doesn't overwrite accumulated counters with zero — `74e6e378`). In the streaming loop, prepended `usage, _ := fallbackStepUsage(stepMessages, stepResult)` so the new estimator (`6ed8852b`) corrects providers that omit final usage chunks. In `Summarize`, kept our `re-fetch session before save` (preserves user todo edits during summary stream) + `IncrementCost` pattern + adopted upstream's `summaryCompletionTokens(usage, summaryMessage)` helper. Took the two new helper functions verbatim. **Rejected**: their `estimated bool` parameter on `updateSessionUsage` (drives the TUI `EstimatedUsage` marker — Section 2 default rule); their `a.eventTokensUsed(...)` publish (no consumer in our WebSocket fan-out) |
| `internal/agent/coordinator.go` (UU + `2faa467a`) | restored ours | upstream `6716ef09` threaded a `skillsMgr *skills.Manager` parameter through `NewCoordinator`; auto-merge left both that param and `skills.DiscoverFromConfig` call in. Stripped the parameter and restored our pre-merge `discoverSkills` body (uses `skills.DiscoverBuiltinWithStates` + `skills.DiscoverWithStates` directly) and its 7-arg `logDiscoveryStats` helper. `2faa467a`'s reasoning-effort guard (`if !model.SupportsReasoningEffort { …only thinking }`) auto-merged cleanly |
| `internal/skills/skills.go` (UU) | combine struct only | kept our `Source` field (drives `DiscoverCommands` scrape of `~/.{claude,gemini,qwen,cursor,zed,windsurf,crush}/commands/` that the WebUI surfaces as slash-commands — fork-only feature), added their two new boolean flags (`UserInvocable`, `DisableModelInvocation`) so SKILL.md files using them parse without error. **Rejected** the rest of their skills-architecture rewrite |
| `internal/skills/{catalog,manager,manager_test}.go` (upstream-added, ~600 LoC) | `git rm` | Manager/Catalog wraps per-workspace state for their multi-client backend (one Manager per workspace + `WithGlobalMirror` fallback for local TUI). We have a single embedded `App` per process and a WebSocket hub for fan-out — there is nothing to manage |
| `internal/proto/skills.go` (upstream-added) | `git rm` | wire types for their REST client-server skills events — no consumer |
| `internal/proto/proto.go` `Workspace.Skills []SkillState` field | dropped | dangling reference to removed `SkillState` |
| `internal/app/app.go` (UU + auto-merged callsites) | keep ours | dropped their `setupEvents() / setupSubscriber[T any](...)` block (forwards service brokers into a bubbletea `tea.Msg` pubsub — TUI infrastructure). Stripped `Skills *skills.Manager` field, `skillsMgr` parameter on `New(...)`, and `app.Skills` argument to `NewCoordinator(...)` — all came in via auto-merge from their multi-client rewrite |
| `internal/cmd/root.go` (UU 3 clusters) | `git checkout --ours` | upstream injected 418 lines of client-server bootstrap (`setupClientServerWorkspace`, `connectToServer`, `ensureServer`, `spawnAndWaitReady`, `localSkillsDiscoveryConfig`) into `setupApp()`. Our HEAD `setupApp()` is the simple embedded path — `appInstance, err := app.New(ctx, conn, store); return appInstance, nil`. Also stripped their imports of `internal/session` / `internal/skills` / `internal/ui/common` / `internal/ui/model` |
| `internal/session/session.go` (UU + 5 auto-merged callsites) | keep ours + cleanup | restored our `ListAll` + value-receiver `fromDBItem`. Stripped the auto-merged `EstimatedUsage` infrastructure that came in as TUI-marker plumbing for upstream `2736e487 fix(ui): mark estimated context usage` + `9595d1f0 fix(session): preserve estimated usage marker`: removed `Session.EstimatedUsage` field, `service.estimatedUsageMu` mutex + `estimatedUsage` map, the three setter methods (`apply` / `set` / `clear`), and the 5 callsites that auto-merge had sprinkled through `Delete` / `Get` / `GetLast` / `Save` / `ListAll`. Section 2 default rule |
| `internal/session/session_test.go` (AA) | `git checkout --ours` | our 242-line in-memory-SQLite test vs their 81-line stub — ours covers more behaviour |
| `internal/server/events.go` + `server.go` (UU) | `git checkout --ours` | upstream rewrote both as REST `/v1/...` proto-conversion helpers (`messageToProto`, `sessionToProto`, `wrapEvent`, `recoverHandler` mux wrapper). Ours are the WebSocket subscriber + handler dispatch. Different architectures. `// Fork merge note` already in place from prior merges |
| `internal/server/events_test.go` + `recover_test.go` | deleted | upstream-added tests for `messageToProto` / `recoverHandler` — both reference functions we never adopted |
| `internal/client/config_test.go` | deleted | orphan test from the deleted `internal/client/` package |

**New rules added to `CHANGELOG.fork.md` Section 2 — "Auto-reject all
upstream TUI / client-server features".** Explicit, named patterns to
catch on future merges (UI files, server/REST stack, Manager/Catalog
abstractions, `setupEvents` + `tea.Msg` bridges, EstimatedUsage-style
TUI marker plumbing, auto-merged callsites of removed types). The
intent is to stop asking the user about TUI/CS additions — the decision
is permanent.

Verification:

- `go build ./...` — clean
- `go vet ./...` — clean (modulo the pre-existing `csync.Map` lock-by-
  value warning unrelated to this merge)
- `go test -count=1 -timeout=300s ./...` — zero regressions vs pre-merge
  snapshot. All post-merge failures (`TestConfig_configureProviders*`,
  `TestGlobWithDoubleStar/*`, `TestListDirectory/*`, `TestStreamExitError`)
  were already failing pre-merge on Windows (Windows path-slash + cred
  resolution flakes). New failures: 0.
- Smoke: built `crush.exe`, ran `crush sessions list` / `sessions locks`
  / `--help` — all clean
- All 42 upstream commits absorbed or explicitly rejected (every
  rejection logged in this batch entry or via `// Fork merge note`
  in the touched file)

Files touched (~25 modified, ~40 deleted, 0 added beyond what upstream
shipped that we kept):

```
internal/agent/agent.go               — 5 conflict clusters resolved (usage accounting)
internal/agent/coordinator.go         — skillsMgr stripped, discoverSkills restored
internal/agent/templates/coder.md.tpl — kept compressed + offset/limit guidance
internal/agent/tools/view.go          — DefaultReadLimit 2000→500
internal/agent/tools/bash.go +safe.go +safe_test.go +bash_test.go — chained-perms fix
internal/agent/usage_fallback.go +_test.go — taken; tests adapted to our 4-arg sig
internal/app/app.go                   — Skills field + setupEvents block removed
internal/cmd/root.go                  — kept ours (CS stack rejected)
internal/permission/permission.go     — atomic.Bool skip + our extra methods
internal/permission/permission_test.go — TestSkipRace adapted to our ctor sig
internal/proto/proto.go               — Workspace.Skills field dropped
internal/server/{events,server}.go    — kept ours (WebSocket)
internal/session/session.go           — EstimatedUsage infra removed
internal/session/session_test.go      — kept ours
internal/skills/skills.go             — Skill struct combined
go.mod / go.sum                       — upstream + fang/v2 + u-root pin
CHANGELOG.fork.md                     — Section 2 auto-reject rule + this batch entry

DELETED:
internal/backend/ (3 files)
internal/client/ (3 files including orphan test)
internal/server/config.go +events_test.go +recover_test.go
internal/skills/{catalog,manager,manager_test}.go
internal/proto/skills.go
internal/ui/ ~22 files across attachments/chat/common/dialog/list/model/styles + 2 new upstream test files
internal/workspace/ (4 files)
internal/cmd/server*.go — already deleted, just kept it that way
.github/cla-signatures.json
```

### Batch 24 — sessions monitoring UX: tool-call previews + watch live-tail (2026-05-26)

Two changes that make watching agent runs actually pleasant. Until now,
following a `crush run` session looked like a wall of `[tool: bash]` /
`[tool-result: bash]` lines — you knew the agent was *doing things*
but not *which things*. And `crush sessions watch` printed a dashboard
of active sessions, which duplicates `sessions list` + `sessions locks`
and is not what an operator usually wants.

**Tool-call argument previews in the message renderer.**

`printMessageWithTime` (text mode) now renders the most informative
argument from each tool call's JSON input next to the tool name. New
helpers `formatToolCallPreview` and `formatToolResultPreview` in
`internal/cmd/sessions_render.go`. Per-tool field priority hand-curated
for the common cases (`bash`→command, `view`→file_path[:offset+limit],
`edit`/`multiedit`/`write`→file_path, `grep`→pattern[+path],
`glob`→pattern, `ls`→path, `fetch`/`web_fetch`/`agentic_fetch`→url,
`download`→url→file_path, `sourcegraph`→query, `agent`/`sub_agent`/`task`
→description (then prompt), `todo`/`todowrite`→item count). Generic
fallback for unknown tools picks the first non-empty string field in
alphabetical order. Rune-truncated to 80 chars (tool calls) / 200 chars
(tool results). Multibyte-safe. Affects `sessions last`, `sessions tail`,
`sessions pick`, `sessions watch <id>` — every text-mode message render
in the project.

Before:

    [tool: bash]
    [tool-result: bash]
    [tool: edit]
    [tool-result: edit]

After:

    [tool: bash] cd D:/dev/go/crush && go build ./... 2>&1 | head -30
    [tool-result: bash] no output
    [tool: edit] D:/dev/go/crush/internal/cmd/queue.go
    [tool-result: edit] <result> (+21 lines)

**`sessions watch` reshaped: drop dashboard, default to picker → live-tail.**

The old dashboard mode (auto-refreshed table of active sessions with
PULSE / AGE / LAST_TOOL / TOKENS / COST columns) was removed — it
duplicated `sessions list` + `sessions locks` and crowded the command
with three modes (no-args / `--pick` / `<id>`) that obscured the actual
job. `watch -n 3 'crush sessions list'` in the shell trivially
reproduces what the dashboard did. Final shape:

    crush sessions watch          → interactive picker, then live-tail
    crush sessions watch <id>     → live-tail directly (short hash OK)

Live-tail prints existing messages, polls every `--interval` (default
1s) for new ones, and exits cleanly when the session ends. End
detection has three independent signals, any of which terminates:

    (a) the session row has a non-empty EndedReason
    (b) the lock file disappeared AND ≥1 message exists (the "≥1
        message" guard avoids racing the acquirer that has opened the
        lock but not yet written its first message)
    (c) the latest assistant message has a non-partial Finish.Reason

The decision lives in the pure helper `isSessionFinishedFromState`
so it is unit-testable without an app / DB / filesystem.

On exit, `formatWatchSummary` renders the end-of-watch block:

    --- session ended ---
    id:       batch30-runaway-fork-tree-queue
    title:    Batch 30: Sessions cancel, fork, tree, queue
    reason:   stop
    duration: 185h30m
    tokens:   183,706 (prompt 183,669 + completion 37)
    cost:     $0.5100

`Ctrl+C` interrupts and prints `(interrupted — session still running)`
*without* a summary — keeps the "I stopped watching" / "the session
ended" distinction obvious. Title line is omitted when empty; budget
suffix appended only when `BudgetMaxCost > 0`.

Drive-by fix in `sessionsLastCmdRun`: the function called
`resolveSessionID` to map a short hash to a full ID and then *ignored*
the result, passing `args[0]` (the hash) directly into
`Messages.List`. Short-hash invocations returned empty output. Now
uses `sess.ID`. Same fix `sessions tail` already had.

Tests (`internal/cmd/sessions_render_test.go` + `sessions_watch_test.go`):

- `formatToolCallPreview` — 24 table cases (every per-tool branch +
  invalid-JSON fallback + unknown-tool fallback + case-insensitive
  matching + multibyte truncation + empty/whitespace input)
- `formatToolResultPreview` — 7 cases (empty, single-line short / at-200 /
  long, multiline with leading whitespace)
- `truncatePreview` / `stringField` / `intField` — basic invariants
- `isSessionFinishedFromState` — 8 cases (each of the three signals
  individually, the "lock gone but no messages yet" race guard, the
  no-signal live-session case, transient DB errors must NOT terminate,
  Partial=true Finish must NOT terminate, EndedReason wins over
  FinishPart when both are set)
- `formatWatchSummary` — full layout / with budget / no title / no
  CreatedAt (no panic, "0s" duration)
- `formatWatchInt` — thousands separator at every order-of-magnitude
  boundary
- `formatAge` — boundary cases at 60s / 3600s

Build clean, vet clean (modulo the pre-existing `csync.Map` lock-by-
value warning), full `./internal/cmd/...` test suite green.

Files:

```
internal/cmd/sessions_render.go         new — preview helpers
internal/cmd/sessions_render_test.go    new — table-driven preview tests
internal/cmd/sessions_watch.go          rewrite — dashboard mode dropped,
                                        picker→live-tail is the default,
                                        pure helpers isSessionFinishedFromState
                                        and formatWatchSummary extracted
internal/cmd/sessions_watch_test.go     new — finished/summary/age/int tests
internal/cmd/sessions.go                renderer hooks + sessions-last hash fix
CHANGELOG.fork.md                       this entry
```

### Batch 31 — model aliases: top opus → Opus 4.8; ownership markers out of `description` (2026-05-29)

Opus 4.8 shipped. Two related changes across the two model-alias
subsystems: the `claude-init` slash-commands / sub-agents and the
`crush models use` short-code table.

**Top opus shortcuts now resolve to `claude-opus-4-8`.** In
`allModelCommands` (`internal/cmd/claude_init.go`) the top-of-family
opus aliases `ol/om/oh/ox/oxx` — and their agent twins
`aol/aom/aoh/aox/aoxx` — were bumped from `claude-opus-4-7` to
`claude-opus-4-8`. The versioned `o47*` / `ao47*` family stays pinned to
`claude-opus-4-7`, so 4.7 remains reachable by its own prefix now that it
is no longer the top. No `o48*` family was added — 4.8 is reached via the
top aliases, matching the prior arrangement where the top mirrored the
newest version. The display labels in `renderShortCodesBlock`
(`internal/cmd/models_atoms.go`) were updated to match.

**Ownership sentinels moved out of the visible `description`.** The
`<!-- crush-model-command:v1 -->` / `<!-- crush-model-agent:v1 -->`
markers used to sit inside the YAML `description:` field, so they leaked
into the Claude Code command list and the agent picker / tool listing
(e.g. `aoh: <!-- crush-model-agent:v1 --> claude-opus-…`). They are
load-bearing — `claude-init` and `claude-del` both use a whole-file
`strings.Contains(data, sentinel)` to tell our files from foreign ones
before overwriting / deleting — so they can't simply be removed. The
frontmatter must stay on line 1 (a leading comment makes Claude Code fail
to parse the YAML and render that first line as the description — which is
also why `crush.md`'s own leading sentinel shows up as its listed
description), so the marker is now an HTML comment in the body:

- commands have a tiny body (`$ARGUMENTS`), so the marker is buried ~50
  blank lines down (`sentinelBodyGap`) to stay out of the command-body
  preview as well as the description;
- agents already have a long body, so the marker sits at the very end.

The `Contains` ownership check is unchanged (it scans the whole file), so
overwrite-protection and old-format GC still work — only the UI is
cleaner.

Drive-by: fixed a pre-existing gofmt misalignment in the `atom` struct in
`models_atoms.go` (whitespace only).

Build / vet / gofmt clean; `./internal/cmd/...` and `./internal/agent/`
test suites green. The generated `.claude/**` files are gitignored — the
source of truth is the Go generator; run `crush claude-init` (`--global`
for `~/.claude/`) to refresh a workspace.

Files:

```
internal/cmd/claude_init.go     top opus aliases (ol..oxx / aol..aoxx) → 4.8;
                                sentinels moved out of description (end of
                                command + agent body); batch-31 header note
internal/cmd/models_atoms.go    short-code table top opus → 4.8; atom struct
                                gofmt realign
CHANGELOG.fork.md               this entry
```
