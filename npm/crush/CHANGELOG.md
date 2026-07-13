# Changelog

All notable changes to the `@phpcraftdream/crush` npm distribution are
documented here. Versions correspond to the `npm-vX.Y.Z` git tags that
trigger [`publish-fork-npm.yml`](../../.github/workflows/publish-fork-npm.yml).

This is the npm-package changelog, not the fork's engineering decision
log — see [`CHANGELOG.fork.md`](../../CHANGELOG.fork.md) at the repo
root for the full per-file merge/divergence history.

## [0.1.7]

- Fixed: a tool call with malformed JSON arguments from the model used to be
  persisted as-is and re-read from the DB every subsequent turn, sticking the
  session forever. Malformed input is now sanitized before storage and the
  matching tool result is turned into an explicit "arguments were not valid
  JSON" error the model can react to.
- Fixed: a non-vision model receiving media (an image) back from a tool call
  could brick the session instead of continuing. It now gets a text
  placeholder in place of the media it can't process.
- Fixed: switching to a different provider/model with no reasoning effort of
  its own could silently keep the previous provider's effort level instead of
  resetting to the new model's own default.
- `crush`'s reasoning-capable API providers now fall back to the first
  available reasoning level instead of silently running without reasoning
  when neither the user nor the model config specifies an effort.
- Z.AI/GLM: an unset reasoning effort now defaults to thinking **on** at
  `high` instead of off (Z.AI recommends max/high for coding tasks, and
  GLM-5.x only exposes high/max — no lower tier to "fall back" to). Opt out
  explicitly with an effort of `off`. DeepSeek is unaffected and keeps its
  original "unset = no reasoning" default.
- Fixed: abbreviated directory names (e.g. in `~/.../p/file`-style paths)
  took the first *byte* of a non-ASCII name instead of the first character,
  mangling Cyrillic/CJK/emoji directory names.
- New: a `llama.cpp` model enricher auto-detects context window size from
  the server's `/v1/models` metadata, matching the existing Ollama/LM
  Studio/LiteLLM/oMLX enrichers.
- Every outbound request to the model provider now carries a deterministic,
  opaque session-affinity header, and provider-reported warnings are now
  logged instead of silently dropped.
- Fixed (macOS): project-level skills discovery could duplicate every
  monorepo-root skills directory when the working directory was itself the
  git repository root, because of a symlink-unaware path comparison
  (`/var` vs macOS's real `/private/var`).

## [0.1.6]

- Windows: fixed a real instant-death bug where `crush run` launched detached
  in the background (e.g. `crush run ... > out 2> err &`) could be killed the
  moment the wrapper shell's console closed — Windows sends `CTRL_CLOSE_EVENT`
  to every attached process on console close, and that termination cannot be
  prevented from inside a console-control handler. Fixed by detaching from
  the console (`FreeConsole`) at startup whenever all three std streams are
  redirected.
- Windows: eliminated console-window flashing introduced by the fix above.
  Every child process `crush` spawns (git, MCP stdio servers, ripgrep,
  cliprovider CLI launches, docker, the `--on-finish` hook, `sessions diff`,
  `sessions pick`, `queue`, model-effort probing, `crush run` sub-invocations
  from `queue`, and the `taskkill` used by `sessions kill`/`reap` and stale-
  lock reclamation) now launches with its console window hidden instead of
  momentarily visible.
- `crush sessions why <id>` and `sessions list` no longer misreport a session
  as "crashed" when its lock file's PID can't be read — a normal, expected
  side effect of Windows' mandatory file locking for a genuinely alive
  session, not proof of death. They now fall back to heartbeat freshness in
  that case, while still trusting a confirmed-dead PID unconditionally.
- `crush run`: fixed a race where a peak-hours window opening mid-turn could
  abort the run without a `RESUME AT` explanation reaching the output — the
  guidance is now printed to stderr and the specific peak-hours error message
  is preserved through the whole cancel/abort path instead of being
  overwritten by a generic "cancelled" message.
- `crush run`: peak-hours is now re-checked mid-turn on a 10s ticker in
  addition to step boundaries, so a long stream, a retry loop, or a
  long-running tool call can no longer run straight through a peak-hours
  window opening without being interrupted. The check also reloads the
  provider config from disk when it's changed since the turn started, so a
  `peak_hours` edit made from another process (e.g. the web UI or a second
  `crush` invocation) while a run is mid-turn takes effect immediately
  instead of only on the next run.
- Fixed: the built-in `local-cli` provider (the local `claude`/`gemini`/
  `codex`/`qwen` CLI models) silently lost `peak_hours`, `disable`, and any
  custom display name on every single config load and reload — it was being
  rebuilt from a bare template each time instead of preserving the existing
  entry. This meant `crush providers set local-cli --peak-hours ...` was
  accepted and persisted to disk but never actually took effect. Found via a
  live test of the peak-hours mid-turn refresh above; predates that feature
  entirely.
- Windows: an interactive `crush run` (no redirected stdio) could sometimes
  cancel with a bare "context canceled" on ordinary console events — Windows
  maps `CTRL_CLOSE_EVENT`/`CTRL_LOGOFF_EVENT`/`CTRL_SHUTDOWN_EVENT` to the
  same signal Go's runtime uses for a real Ctrl+C. Only a genuine Ctrl+C now
  cancels the run.
- `crush run`: fixed handling of busy sessions and stale locks — a
  session already busy in-process no longer surfaces as a bare nil result,
  and lock reclamation on a stale-but-contended heartbeat retries instead of
  spuriously reporting "busy".
- Web UI: sessions started or driven from the web UI now auto-approve every
  tool permission, matching how non-interactive `crush run` already behaves.
  The permission-request dialog can no longer appear in the web UI; the
  now-unreachable dialog component, its WebSocket events/handlers, and the
  backend endpoints that served it were removed.

## [0.1.5]

- Web UI: the Providers settings modal is now the single place to edit every
  provider parameter, including the API key, for both custom and built-in
  providers (anthropic, openai, zai, ...) — previously a built-in provider's
  key could only be set from the model-selection dropdown, and its other
  fields weren't editable at all. The now-redundant "Edit key" / "Remove
  key" / "+ Add API key" affordances were removed from model selection.
- Web UI: providers without an API key configured no longer clutter the
  model-selection dropdown (CLI-type providers, which don't need a key, are
  unaffected).
- Web UI: configured providers (API key set) now sort to the top of the
  Providers settings list.
- Web UI: peak-hours start/end are now a plain 24-hour `HH:MM` text field
  instead of the native time picker, which showed AM/PM on some non-Chromium
  browsers regardless of locale.
- Web UI: a global/local scope selector on add/edit/remove for custom
  providers, and peak-hours management for built-in providers — previously
  only custom providers could be scoped.
- Web UI: the background "keep Bluetooth headphones awake" noise loop now
  stops while the backend is disconnected and resumes automatically on
  reconnect, instead of playing pointlessly against a dead connection.
- `crush ping` now shows a provider's peak-hours status in its output.
- `crush run` now has a default 6-hour hard wall-clock backstop when
  `--timeout` is unset or 0 (override via `CRUSH_RUN_DEFAULT_HARD_TIMEOUT`),
  so a run can no longer hang indefinitely with no timeout at all.
- `crush mcp add/remove/enable/disable/set` and `crush claude-init`/
  `claude-del` now default to the global scope with an explicit `--local`
  flag to opt into project-local, matching every other scoped command
  (previously inconsistent — some defaulted to local with no way to
  target global from the flag).
- Per-model slash-commands installed by `cah install` (e.g. `/oxx`, `/sh`,
  `/fl`) are no longer surfaced in crush's own skill/command discovery —
  those pin a specific model to switch to and aren't general-purpose
  commands.
- Fixed: loop detection could trip on a step *after* the one that actually
  repeated, due to an ordering mismatch between the fantasy SDK's
  `OnStepFinish` and `StopWhen` callbacks; it now stops on the exact step
  that trips the detector and records a distinguishable "stopped by
  loop-detection" message.
- Fixed: a CLI provider's background process wait could block forever if a
  grandchild process held stderr open, and its kill only terminated the
  direct child on Windows, orphaning `node.exe`. Both are now bounded/tree-
  killed correctly.
- Fixed: `crush sessions why <id>`'s verdict could disagree with `sessions
  list` for a stale-lock-but-cleanly-finished session.
- Local/development builds now report a version like
  `<upstream-tag>-<commit>-0.1.5` (e.g. `v0.72.1-06c8078-0.1.5`) — the
  upstream base tag is preserved, and neither a `devel` nor a `dirty`
  marker is ever included.

## [0.1.4]

- New per-provider `peak_hours` refusal window: a provider can be configured
  with a local-time `{start, end}` window (overnight wrap supported) during
  which `crush run` refuses to use it, with a clear text error naming the
  provider and when it becomes available again. Manageable from `crush
  providers set/add --peak-hours HH:MM-HH:MM`, `show`, and `list`, from the
  web UI's provider editor, and over the WebSocket provider API.
- New `crush run --allow-peak-hours` flag to bypass a provider's peak-hours
  refusal for a single invocation. This is a conscious one-off override with
  no persistent config equivalent, and its `--help` text carries an explicit
  warning that an orchestrating agent must never add it unsolicited — only
  on a human operator's explicit request for that specific run.
- New `crush sessions why <id>` command: a one-shot diagnostic explaining
  whether a session is running, crashed, done, or at rest, using only the
  session DB and lock-file state — including reclassifying a "crashed" lock
  as done when the last assistant message actually finished cleanly.
- New `--color-scheme light|dark|auto` flag and `CRUSH_COLOR_SCHEME` env var
  to force the CLI help/error color palette, working around terminals where
  automatic light/dark background detection is unreliable or unavailable
  (e.g. redirected stdio, or a terminal that doesn't answer the background
  color query in time).
- Fixed: a malformed `peak_hours` time string could previously parse into a
  plausible-but-wrong time instead of being rejected.
- Fixed: a background job's forceful termination could hang the whole agent
  turn well past the configured tool-execution watchdog cap when the
  underlying process ignored cancellation.
- Local/development builds now report a version like
  `devel-<commit>-0.1.4[-dirty]` instead of a bare `devel-<commit>[-dirty]`,
  and no longer show a raw, unhelpful Go pseudo-version timestamp when one
  leaks through from a `go install`-style build.

## [0.1.3]

- New opt-in restricted permission model for `crush run`: `permissions.run`
  config plus `--restrict-run` / `--allow-bash` / `--allow-tool` flags.
  When armed, a non-interactive run switches from auto-approve-everything
  to deny-by-default, gating each tool/bash call against an allowlist.
- Bash allowlist patterns (`cmd args` prefix, `exact:`, `glob:`, `regex:`)
  are compound-guarded via a real shell parse: a permissive pattern can
  never authorise a chained/backgrounded/substituted command (e.g.
  `git status && rm -rf /` or `git status\nrm -rf /`). Globs are matched
  cross-platform.
- Hardening of the interactive bash safe-read-only fast-path: the same
  shell-parse compound check now gates it, closing a bypass where a
  newline- or `&`-chained command behind a safe prefix ran without a
  permission prompt.

## [0.1.2]

- Documented `crush sessions inject` (cross-process message injection,
  merge and `--interrupt` modes) in the `/crush` slash-command guide —
  the feature had shipped without the corresponding doc update.
- 0.1.0 and 0.1.1 were unpublished from npm; 0.1.2 is the first
  version to install cleanly going forward.

## [0.1.1] (unpublished)

- `crush models list` no longer hits the network or writes the provider
  cache by default; pass `--refresh` to force a fresh fetch.
- `crush ping --role smart|fast` — ping either model slot without
  switching to `ping-fast`.
- `ZHIPU_API_KEY` accepted as a fallback for the Z.AI provider when
  `ZAI_API_KEY` is unset.
- Startup diagnostics ("no git repository detected", "Apple Terminal
  detected") no longer print on scripted/default runs — only under
  `--debug` (or `"options": {"debug": true}` in config).
- Hardened version reporting: an ldflags-injected release version can no
  longer be silently overwritten by Go's module metadata, and the npm
  publish workflow verifies the packaged binary reports the expected
  version before publishing.

## [0.1.0] (unpublished)

- First release of the npm distribution: `npm install -g
  @phpcraftdream/crush` installs a prebuilt, Go-free binary via
  per-platform optional dependencies (linux/darwin × x64/arm64,
  win32 x64).
