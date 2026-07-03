# Crush — Fork-Specific Merge & Development Guide

This file is **fork-local guidance** (not committed upstream). The fork
(`PHPCraftdream/crush`) has diverged substantially from
`charmbracelet/crush` and is **NOT a passive mirror**. Treat upstream as
an external project whose changes are imported selectively, never as a
source of truth.

## The Golden Rule

**Fork changes always win.** When upstream and fork disagree, the fork's
version is correct by default. Upstream changes are imported only when:

1. They fix a real bug that also affects the fork, AND
2. They do not regress fork-specific functionality, AND
3. They do not touch a subsystem the fork has rewritten or removed.

If any of those fails, **drop the upstream commit** with a one-line note
in the merge commit (`SKIP <hash>: <reason>`).

## What This Fork IS — Repositioned Identity

The fork is positioned as **agent-tooling**: a CLI optimised for
delegation from Claude Code / orchestrators / scripts. The crown jewels
are:

- `crush run` non-interactive entry point + agent harness
- `sessions` subcommand family (pick / watch / tree / kill / locks /
  purge / reap / gc / grep / diff / cost / fork)
- `cliprovider` — invoke local `claude` / `gemini` / `codex` / `qwen`
  CLIs as fantasy.Provider
- `agentguard` — block sub-agents from touching shared git/state
- `agentic_fetch_tool`, `hooked_tool`, `loop_detection`,
  `stream_watchdog`, `usage_fallback`, `notify`
- `claude-init` / `claude-del` — install slash-commands + sub-agents
  into `.claude/{commands,agents}/`
- Per-model atoms + short codes (`fl`, `ox`, `s46xx`, …) in
  `cmd/models_atoms.go`
- React web UI under `web/` (Playwright e2e) — replaces upstream TUI
- Pre-push hook mirroring CI under `.githooks/pre-push`

## What This Fork REMOVED — Do NOT Re-import

Upstream commits touching any of these MUST be skipped:

| Removed subsystem | Upstream-coded as | Skip rule |
|---|---|---|
| **Bubble Tea TUI** | `internal/tui/` | Any commit under `internal/tui/**` → SKIP |
| Upstream YOLO + permissions test files | `b46dae6c` removed them | SKIP fixes there — fork has no YOLO/auto-approve UI at all (removed `5c323b55`); only non-interactive `crush run` auto-approve remains |
| Taskfile.yaml, swagger stub, nix, playwright temp | `52bb90f8` | SKIP CI/build replays |
| CLA bot infra | `chore(legal): @… signed CLA` commits | SKIP — fork doesn't use CLA bot |
| Auto-update / scheduled chores | `chore: auto-update files`, cron `nightly.yml` | SKIP |
| `--effort high` thinking model variants (deprecated) | `8b3707dd` | SKIP attempts to re-add |
| localStorage settings (we use backend) | `8868dced` | SKIP UI commits that re-introduce client-side persistence |

## What This Fork OWNS — Touch Upstream Only With Extreme Care

These subsystems are fork-rewritten. Upstream changes to the same area
are almost always wrong for us:

### Multi-client / multi-session
- **Upstream** is shipping a new multi-client server model (look for
  `feat(server): share one workspace per directory across clients`,
  `feat(db): refuse to open a data directory in use by another crush`,
  `feat(tui): auto-close permission prompt when another client
  responds`, etc.). **Skip all of it.**
- **Fork** uses lock-file + heartbeat model in `sessions_*.go`
  (10s touch, 20s expiry, auto-clear stale locks > 60s, `kill`
  enforces taskkill /F /T on Windows). This is intentional and
  battle-tested.

### Web UI / WebSocket
- **Upstream** has no web UI — they own the TUI.
- **Fork** replaced TUI entirely with React (`web/`) + WebSocket
  server. Any upstream `feat(ui)` / `fix(ui)` is for TUI and does
  not apply.
- Fork-side: `feat(web): …` commits. Upstream-side: `feat(tui): …`.
  Mapping is rarely 1:1.

### Agent / coordinator / CLI provider
- **Fork** has rewritten parts: `agentguard`, `cliprovider`, `hyper`,
  `agentic_fetch_tool`, `hooked_tool`, `loop_detection`,
  `stream_watchdog`, `usage_fallback`, `notify`, `prompt`.
- Upstream `fix(agent)` may apply if the fix targets shared code in
  `agent.go` / `coordinator.go`, but **always cherry-pick a clean
  patch** rather than accepting a merge that drags in adjacent files.
- Specifically: upstream's `fix(agent): centralize 401 retry logic`
  / `add 401 retry and reauth notification to sub-agent runs` /
  `fix(oauth): …` may be worth porting **if** the fork's oauth
  layer is identical. Check first.

### Provider-model registry
- **Fork** uses `cmd/models_atoms.go` (atoms + short codes:
  `fl`/`oh`/`s46xx`/...) and a curated `atomRegistry`.
- **Upstream** uses raw catwalk lists. Their `fix(cli): show all
  providers in crush models` already landed in spirit on the fork.
- New provider integrations from upstream (alibaba, bedrock europe,
  copilot models, qwen3.7-max fixes, …) → port the **client logic**
  only, leave atom registry to the fork.

### Slash-commands / agents installer
- **Fork**: `cmd/claude_init.go` + `claude_del.go` install `/crush`
  and per-model commands (`fl`, `ox`, etc.) plus sub-agents into
  `.claude/{commands,agents}/`.
- Upstream has nothing equivalent. Any "skills discovery" upstream
  change (`feat: discover skills from git root in monorepos`) is
  independent and may be safe to port.

### CI / hooks
- **Fork** has its own `nightly.yml` (cron disabled),
  `security.yml`, `.githooks/pre-push` mirroring CI.
- **Upstream** CI updates → review individually. Most are noise.
  Do NOT enable any cron on the fork (we explicitly disabled all
  scheduled workflows to stop emails).

## Merge Workflow

When the user asks to merge upstream:

1. **Fetch first**: `git fetch origin main` (origin = upstream
   charmbracelet, fork = our PHPCraftdream remote).
2. **Triage by category** before opening any merge:
   ```bash
   git log --oneline --no-merges origin/main ^main
   ```
   Bucket every commit into:
   - **PORT** — bug fix in shared code, no TUI/server/web touch
   - **EVAL** — provider/agent change that may apply, needs read
   - **SKIP** — TUI, multi-client server, CLA, auto-update, cron,
     anything in the "REMOVED" or "OWNED" tables above
3. **Cherry-pick the PORT bucket** one commit at a time. Do not run
   a bulk `git merge origin/main` — it drags in everything and
   creates conflicts in subsystems we own.
4. **For each ported commit**: rebuild, run `go test ./...`, run
   the pre-push hook (`.githooks/pre-push`). If anything breaks,
   bail on that commit and write a SKIP note.
5. **Document skips** in the merge commit message. Future-us needs
   to know why specific upstream commits never came over.

## Subagents Working on This Repo

If you are a delegated worker (Task tool / sub-agent), in addition to
the shared-workspace git-safety clause already in your prompt:

- **Do not bulk-merge upstream.** Ever. Even if asked. Surface the
  list, ask for confirmation per category.
- **Do not run project-wide test suites** (`go test ./...`,
  `make test`). Test only the packages you touched. The orchestrator
  runs the full suite at the end.
- **Do not touch CLAUDE.md** (this file) unless explicitly told to.
- **Do not touch `.github/workflows/`** to re-enable cron schedules.
- **Do not delete `web/dist/.gitkeep`** — it's required by
  `//go:embed all:dist` for CI builds. (It is currently deleted in
  the working tree; user knows; do not commit the deletion either
  way without explicit instruction.)

## Quick Reference — Where Things Live

| Concern | Fork file |
|---|---|
| Atoms / short codes | `internal/cmd/models_atoms.go` |
| CLI providers (claude/gemini/codex/qwen) | `internal/agent/cliprovider/provider.go` |
| Ping (incl. CLI providers) | `internal/cmd/ping.go` |
| Sessions CLI family | `internal/cmd/sessions_*.go` |
| Slash-command / agent installer | `internal/cmd/claude_init.go`, `claude_del.go` |
| Slash-command body | `internal/cmd/claude_slash_command.md` |
| Agent guard rails | `internal/agent/agentguard/` |
| Stream watchdog | `internal/agent/stream_watchdog.go` |
| Loop detection | `internal/agent/loop_detection.go` |
| LSP manager (circuit-breaker) | `internal/lsp/manager.go` |
| Web UI | `web/` |
| Pre-push hook | `.githooks/pre-push` |
