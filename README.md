# Crush — this fork

> **Heads-up:** this repository is a long-running fork of
> [`charmbracelet/crush`](https://github.com/charmbracelet/crush). The
> upstream is a terminal-UI coding agent for humans. **This fork
> specialises in being a tool that other AI agents drive.** If you
> want a polished TUI to chat with, go upstream. If you want a binary
> that an orchestrator (Claude Code, your own LLM wrapper, a CI job,
> a multi-agent fleet) can spawn 5+ times in parallel against the same
> repo and reliably parse the result of, this fork is built for that.

## What this fork actually is

The product is **`crush` as an agent's hands**, not as a human's coding
companion. Every divergence below follows from that single repositioning:

- The TUI is gone — a human is no longer the primary user of the
  process. A React/Tailwind **web UI** stays for the cases where a
  human DOES want to look in, but the design centre is the CLI.
- The CLI is the contract. `crush run` exposes a **wrapper-stable JSON
  envelope** with a small, frozen set of fields an orchestrator parses
  without surprises. New flags (`--role`, `--session`, `--format`,
  `--agents`, `--timeout`, …) all exist to give the upper LLM precise
  control over a delegated turn.
- Multiple instances are a first-class concern. Five `crush run` against
  one `.crush/` directory cannot corrupt each other's state — sessions,
  cost accounting, log writes, MCP-id files and SQLite are all
  defended explicitly.
- Honest error reporting. When the model fails its contract (returns
  invalid JSON, runs out of context, stalls the stream) the envelope
  says so — there is no silent success because the agent on top
  cannot read the operator's mind.
- Bootstrap helpers (`crush claude-init`) drop a delegation guide into
  the workspace so the upper LLM knows when and how to delegate to
  `crush run` instead of grepping the codebase itself.

The browser UI is the second-class entry point for humans peeking in,
the orchestrator-facing CLI is first-class.

| Area | Upstream | This fork |
| ---- | -------- | --------- |
| Primary user | Human in a terminal | Another agent (LLM, CI, orchestrator) calling the CLI |
| Front-end | Bubble Tea TUI (~495 files under `internal/ui/`) | React/Tailwind SPA in `web/`, embedded via `go:embed`. Optional. |
| Transport | REST `/v1/...` over Unix socket / Windows named pipe | WebSocket `/ws` over TCP loopback (single embedded server) |
| Auth | None (local-socket trust) | Token-based, see `internal/server/auth.go` |
| Sessions | One model per agent role, set globally | Per-session model overrides + per-session system prompt + per-session YOLO flag, all persisted in SQLite |
| Permissions | In-memory rules during a TUI run | Persistent per-session rules in SQLite; cross-process visible |
| Parallel runs | Not a target | First-class — flock per session, OS-level lock release on crash, atomic file writes, additive cost SQL, MCP-id flock |
| `crush run` | Single-shot quick fire | Wrapper-friendly: `--role`, `--session` get-or-create, `--json`/`--format`/`--agents`/`--timeout`/`--stream`, JSON-envelope validation, `assistant_notes`, fallback error messages |
| CLI providers | Limited bridge | npx Claude Code, Gemini CLI, Codex CLI, MCP bridge for external tools, session resume for Anthropic prompt caching; Haiku available as `local-cli/cli-claude-haiku` (200k ctx, `@low\|medium\|high` effort) |
| Web UI features | n/a | Slash-command + skill autocomplete, dark/light theme, pinned messages, fork-session button, LSP/MCP/provider management modals, file/image attachments |

The full per-file decision log lives in [`CHANGELOG.fork.md`](./CHANGELOG.fork.md).
That document is also the survival guide for merging upstream `main`
into the fork — every divergence is annotated with a `// Fork patch:`
comment in the code so conflicts surface at the right line.

## Running Crush in this fork

Two complementary entry points; pick whichever fits the job.

### 1. `crush web` — the browser UI

```bash
crush web                            # default port + open browser
crush web --port 8080 --no-open      # for a remote workstation
```

A long-lived process. Sessions live in `.crush/crush.db`, the UI loads
the React bundle from inside the binary, the WebSocket is local-only +
token-authed. This replaces upstream's TUI.

### 2. `crush run` — the orchestration CLI (the main thing)

The canonical pattern an orchestrator should be writing:

```bash
out=/tmp/audit-A.json
CRUSH_FORBID_WRITES="$out" \
  crush run --role smart --session "audit-A" \
            --json --format json --timeout 10m \
            < /tmp/audit-A.prompt > "$out" 2>"$out.err"
jq -r '.exit_reason' "$out"   # "end_turn" on success, "invalid_json" if model broke contract, "error" otherwise
jq -r '.final_text'   "$out"  # the raw JSON the model produced (validated)
jq -r '.assistant_notes' "$out" # any prose preamble that was stripped
jq -r '.error' "$out"         # error.message if non-success
```

#### Flags

- **`--role smart|fast` (required)** — no silent default to the
  expensive model.
- **`--session <id>`** — get-or-create. Pass the same id again to
  continue, or a new id to start fresh. Works as a stable key for CI
  matrices and orchestrator wrappers.
- **`--json`** — emits a single wrapper-stable envelope on stdout:
  `{session_id, exit_reason, final_text, assistant_notes, stripped_bytes,
  tool_calls, usage, duration_ms, error, warnings}`.
- **`--format json | json-schema:<f> | @<f> | <any text>`** — appends a
  per-turn output-shape hint to the prompt AND post-validates `final_text`.
  With `json` or `json-schema:`, the envelope is also post-processed:
  markdown fences and prose preamble are stripped; `json.Valid` runs on
  what remains. **If the model returns syntactically broken JSON**
  (e.g. forgot a `]` somewhere), `exit_reason="invalid_json"` is set,
  the original (unstripped) text is preserved in `final_text`, the
  failed strip attempt goes to `assistant_notes`, and `error` carries
  a `json.SyntaxError` with a byte offset. Wrappers can branch on
  `exit_reason` instead of trusting the model's optimistic `"stop"`.
- **`--agents single | with-agents | agent-allow`** — sub-agent fan-out
  policy. `single` removes the `agent` and `agentic_fetch` tools from
  the toolset entirely so the model literally cannot dispatch
  sub-agents. `with-agents` nudges the model to fan out. `agent-allow`
  (default) leaves the choice to the model.
- **`--aggregation summary | concat | attach`** — how sub-agent fan-out
  output reaches the orchestrator. `summary` (default) lets the parent
  compose a wrap-up; detail lives in the DB only. `concat` adds a
  prompt nudge so the parent includes each sub-agent's reply verbatim
  in `final_text`. `attach` collects each sub-agent's last assistant
  text into `envelope.sub_agent_outputs` so the orchestrator gets the
  structured set; `final_text` becomes a brief wrap-up. An always-on
  warning fires in `envelope.warnings` when parent collapses sub-agent
  outputs to <40% of their combined character count, regardless of
  which mode is in use.
- **`--timeout <duration>`** — hard wall-clock cap; the partial answer
  is preserved in the session and surfaced in the envelope.
- **`--timeout-extends-on-progress`** — when set, the stream watchdog
  resets its idle deadline every time streaming activity occurs, so
  long compositions (code generation, multi-section reports) are not
  killed prematurely. Capped by `--timeout-hard-cap` if set.
- **`--timeout-hard-cap <duration>`** — maximum wall-clock time the
  watchdog will allow even with `--timeout-extends-on-progress`.
  Without a cap a continuously-streaming response runs forever.
  Typically set to 3–4× the idle timeout.
- **`--system-prompt[-file]`** — persists onto the session so follow-up
  runs inherit it.
- **`--stream`** — streams every token to stdout for live wrappers.

#### Envelope fields worth knowing

- `stripped_bytes` — how many bytes were removed from `final_text` by
  the JSON stripper (when `--json`+`--format json` were active). Graph
  it across runs to track how often your model wraps in prose.
- `tool_calls: [{name, count}]` — post-hoc inventory of what tools the
  model actually used. Useful to verify `--agents single` actually
  blocked fan-out.
- `sub_agent_outputs[]` — present only with `--aggregation attach`.
  Each entry is `{session_id, title, final_text, char_count}` for one
  sub-session the parent's `agent` tool dispatched during this run.
- `warnings[]` — non-fatal observations. Includes `final_text appears
  truncated` when the run errored mid-composition (so the operator
  sees the model was about to continue); `final_text is empty after N
  sub-agent fan-out call(s)` when the model dispatched sub-agents but
  never composed a top-level reply; and `reduction-loss: final_text
  is X% of N combined sub-agent chars` when the parent over-summarised
  (re-run with `--aggregation=attach` or `concat` to recover).
- `error` — present whenever `exit_reason` is non-success. If the
  provider's Finish part had no message (some providers emit a bare
  error finish), a fallback names the most likely causes (provider
  HTTP error, stream stall, OOM, context overflow).
- `recovered_partial` — present when the session had an orphaned
  partial assistant message from a previous interrupted run (detected
  by `Finish{Partial: true}` on an unfinished row). Shape:
  `{message_id, chars, last_flush_at, text}`. An always-on WARN in
  `warnings[]` fires when this field is populated: *"recovered N chars
  of partial assistant text — model run was interrupted"*. The text
  may be incomplete but is usually the bulk of what the model produced
  before the kill.

#### Env-vars to know

- **`CRUSH_FORBID_WRITES`** — comma-separated paths the `write`/`edit`/
  `multiedit` tools must NOT touch. **Set this to the stdout-redirect
  target before every `crush run`** — otherwise the model can pick the
  same filename it sees in the prompt and overwrite your envelope
  output. Tool calls to forbidden paths fail visibly to the model;
  it then falls back to returning content via `final_text`.
- **`CRUSH_PROVIDER_CACHE_TTL`** — duration (`24h` default, `0s` to
  always refresh). Caches the Catwalk/Hyper provider catalog locally
  so `crush models show` and similar read-only commands skip the
  ~3-second HTTP round-trip when the on-disk cache is fresher than
  the TTL.

Permissions are auto-approved in `crush run` (non-interactive — no
one is on the keyboard). Mirror this with `--cwd /tmp/sandbox` or a
worktree when running model-written code against shared state.

### 3. Parallel processes against one `.crush/`

The fork explicitly supports running 5+ `crush run --session X` against
the same working directory concurrently (the canonical use case is
multi-section code audits). The defence layers:

- Per-session OS flock (`internal/session/lock.go`) — two processes
  cannot share a session id.
- SQLite WAL + `busy_timeout=30000` + single-writer-per-process
  connection pool.
- Cost mutations go through additive SQL (`IncrementSessionCost`) so
  concurrent sub-agent goroutines AND parallel processes cannot lose
  cost via read-modify-write.
- Atomic file writes (`fsext.AtomicWriteFile`) in `write`/`edit`/
  `multiedit` tools — `kill -9` mid-write cannot truncate the user's
  file.
- Per-process `pid=N` attribute in every log line — interleaved Windows
  log writes can be split post-hoc with `jq 'select(.pid==N)'`.
- Permission grants ("Always allow") are DB-direct on every check, so
  a grant made in process A is immediately visible in process B
  without restart.
- MCP `qwen/gemini-mcp-id` and `~/.{qwen,gemini}/settings.json` writes
  are flock'd with a 30s timeout so a wedged sibling cannot freeze
  the fleet.

See [`CHANGELOG.fork.md`](./CHANGELOG.fork.md) Section 4.I for the full
parallel-process audit and the trade-offs we explicitly kept (e.g. N
processes still spawn N stdio children of every configured MCP server
— use HTTP/SSE-transport MCPs in parallel runs).

### 4. Bootstrap helpers for an orchestrator

If you drive Crush from another LLM (e.g. Claude Code), run once:

```bash
crush claude-init                 # install or refresh the delegation guide in ./CLAUDE.md
```

This drops two things into the workspace:
- A versioned block in `CLAUDE.md` teaching the upper LLM when and how
  to delegate work to `crush run` (channels, `--role`, `--session`
  conventions, backgrounding rules, the `CRUSH_FORBID_WRITES`
  pattern, when to use `--format json` and `--agents single`).
- A `.claude/commands/crush.md` slash command that says "for this task,
  delegate via `crush run` per the rules in CLAUDE.md".

The block is versioned (`<!-- crush-claude-init:vN -->`). Re-run
`crush claude-init` at any time to refresh it — every invocation strips
all prior versions and writes a fresh one. To undo, `crush claude-del`
removes the block and the slash command cleanly.

### 5. `crush models` — picking and inspecting models

Three commands cover the whole surface:

```bash
crush models list           # show available atoms + raw provider/model ids
crush models use <large> <small> [--global | --local]
crush models state          # what's effective + per-scope breakdown (alias: `show`)
```

**Atoms** are short, friendly aliases. `list` prints them filtered by your
currently-enabled providers — disabled providers' atoms are hidden so the
list only shows what actually works right now:

```
ATOMS (combine as `crush models use <large> <small>`):

  Anthropic:
    via local `claude` CLI
    opus-low, opus-medium, opus-high, opus-xhigh, opus-max            Claude Opus    (1M ctx)
    sonnet-low, sonnet-medium, sonnet-high, sonnet-xhigh, sonnet-max  Claude Sonnet  (1M ctx)
    haiku-low, haiku-medium, haiku-high, haiku-xhigh, haiku-max       Claude Haiku   (200k ctx)

  Zai:
    openai-compat, no effort
    glm5_1        GLM 5.1      (204.8k ctx)
    glm5          GLM 5        (204.8k ctx)
    glm5_turbo    GLM 5 turbo  (200k ctx)
    ...
```

Anthropic atoms require a level suffix (`opus-high`, `sonnet-low`, etc.) —
the level list comes from parsing `claude --help` at first use, so it stays
correct as Anthropic adds tiers. Z.AI atoms do not accept levels because
Z.AI via openai-compat doesn't expose an effort parameter.

```bash
crush models use opus-high glm5_turbo                # mixed Anthropic large + Z.AI small
crush models use --local glm5_1 glm5_turbo           # workspace-only override
crush models use openai/gpt-5@high zai/glm-5-turbo   # raw provider/model fallback for anything not in the atom list
```

`models state` shows the currently-effective pair and the per-scope
breakdown so you always know whether your `--local` workspace overrides
your global default or vice versa.

> **Removed in batch 11:** `crush models set --large X --small Y` and the
> entire `crush models preset` subtree (save/use/list/delete). Both
> commands now print a redirect notice pointing at `crush models use`.

To clear an override and fall back to the other scope: `crush models unset
[large|small|both] [--local|--global]`. Defaults to clearing both slots in
the global scope. Missing keys are a no-op.

## When NOT to use this fork

- **You want the TUI experience.** Use upstream — the fork removed it.
- **You want a stable, blessed-by-Charm distribution path.** This fork
  does not publish Homebrew/winget/AUR releases.
- **You want the official REST `/v1/...` protocol for wrapping.** This
  fork speaks WebSocket only.
- **You're a human typing into one terminal session at a time.**
  Upstream's TUI is genuinely nicer for that. This fork's CLI is shaped
  for scripts and orchestrators; the web UI is for peeking in, not for
  daily-driving conversational work.

## When this fork is exactly the right tool

- You're building a multi-agent system where one LLM delegates code
  work to another. `crush run` is that worker; the envelope is the
  protocol between them.
- You run a multi-section audit / refactor / migration as 5+ parallel
  `crush run` invocations against one repo and need the cost
  accounting + lock-file + atomic-write guarantees that follow.
- You wrap LLMs in CI: stable `--session` key per build matrix,
  `--timeout` for budget control, `--json` for jq-parseable output,
  `--format json` for raw JSON contracts with validation.
- You want a long-running embedded coding agent reachable over a
  browser-served WebSocket from a thin React UI.

---

The original upstream README follows below, kept verbatim because most
of its content (installation, configuration, MCP/LSP setup, model
providers) applies unchanged to this fork. Where the fork diverges,
either the text above or `CHANGELOG.fork.md` overrides.

---

# Crush (upstream)

> Logo, release badge, build-status badge and demo GIF removed — they
> point at upstream `charmbracelet/crush` artifacts (Charm's logo,
> upstream's GitHub Actions status, upstream's release tag) and would
> misrepresent this fork's identity, release cadence and CI status.
> The text below is the upstream README's prose, kept verbatim because
> the installation / configuration / providers material applies to
> this fork unchanged.

## Features

- **Multi-Model:** choose from a wide range of LLMs or add your own via OpenAI- or Anthropic-compatible APIs
- **Flexible:** switch LLMs mid-session while preserving context
- **Session-Based:** maintain multiple work sessions and contexts per project
- **LSP-Enhanced:** Crush uses LSPs for additional context, just like you do
- **Extensible:** add capabilities via MCPs (`http`, `stdio`, and `sse`)
- **Works Everywhere:** first-class support in every terminal on macOS, Linux, Windows (PowerShell and WSL), Android, FreeBSD, OpenBSD, and NetBSD
- **Industrial Grade:** built on the Charm ecosystem, powering 25k+ applications, from leading open source projects to business-critical infrastructure

## Installation

Use a package manager:

```bash
# Homebrew
brew install charmbracelet/tap/crush

# NPM
npm install -g @charmland/crush

# Arch Linux (btw)
yay -S crush-bin

# Nix
nix run github:numtide/nix-ai-tools#crush

# FreeBSD
pkg install crush
```

Windows users:

```bash
# Winget
winget install charmbracelet.crush

# Scoop
scoop bucket add charm https://github.com/charmbracelet/scoop-bucket.git
scoop install crush
```

<details>
<summary><strong>Nix (NUR)</strong></summary>

Crush is available via the official Charm [NUR](https://github.com/nix-community/NUR) in `nur.repos.charmbracelet.crush`, which is the most up-to-date way to get Crush in Nix.

You can also try out Crush via the NUR with `nix-shell`:

```bash
# Add the NUR channel.
nix-channel --add https://github.com/nix-community/NUR/archive/main.tar.gz nur
nix-channel --update

# Get Crush in a Nix shell.
nix-shell -p '(import <nur> { pkgs = import <nixpkgs> {}; }).repos.charmbracelet.crush'
```

### NixOS & Home Manager Module Usage via NUR

Crush provides NixOS and Home Manager modules via NUR.
You can use these modules directly in your flake by importing them from NUR. Since it auto detects whether its a home manager or nixos context you can use the import the exact same way :)

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    nur.url = "github:nix-community/NUR";
  };

  outputs = { self, nixpkgs, nur, ... }: {
    nixosConfigurations.your-hostname = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        nur.modules.nixos.default
        nur.repos.charmbracelet.modules.crush
        {
          programs.crush = {
            enable = true;
            settings = {
              providers = {
                openai = {
                  id = "openai";
                  name = "OpenAI";
                  base_url = "https://api.openai.com/v1";
                  type = "openai";
                  api_key = "sk-fake123456789abcdef...";
                  models = [
                    {
                      id = "gpt-4";
                      name = "GPT-4";
                    }
                  ];
                };
              };
              lsp = {
                go = { command = "gopls"; enabled = true; };
                nix = { command = "nil"; enabled = true; };
              };
              options = {
                context_paths = [ "/etc/nixos/configuration.nix" ];
                tui = { compact_mode = true; };
                debug = false;
              };
            };
          };
        }
      ];
    };
  };
}
```

</details>

<details>
<summary><strong>Debian/Ubuntu</strong></summary>

```bash
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://repo.charm.sh/apt/gpg.key | sudo gpg --dearmor -o /etc/apt/keyrings/charm.gpg
echo "deb [signed-by=/etc/apt/keyrings/charm.gpg] https://repo.charm.sh/apt/ * *" | sudo tee /etc/apt/sources.list.d/charm.list
sudo apt update && sudo apt install crush
```

</details>

<details>
<summary><strong>Fedora/RHEL</strong></summary>

```bash
echo '[charm]
name=Charm
baseurl=https://repo.charm.sh/yum/
enabled=1
gpgcheck=1
gpgkey=https://repo.charm.sh/yum/gpg.key' | sudo tee /etc/yum.repos.d/charm.repo
sudo yum install crush
```

</details>

Or, download it:

- [Packages][releases] are available in Debian and RPM formats
- [Binaries][releases] are available for Linux, macOS, Windows, FreeBSD, OpenBSD, and NetBSD

[releases]: https://github.com/charmbracelet/crush/releases

Or just install it with Go:

```
go install github.com/charmbracelet/crush@latest
```

> [!WARNING]
> Productivity may increase when using Crush and you may find yourself nerd
> sniped when first using the application. If the symptoms persist, join the
> [Slack][slack] or [Discord][discord] and nerd snipe the rest of us.

## Getting Started

The quickest way to get started is to grab an API key for your preferred
provider such as Anthropic, OpenAI, Groq, OpenRouter, or Vercel AI Gateway and just start
Crush. You'll be prompted to enter your API key.

That said, you can also set environment variables for preferred providers.

| Environment Variable        | Provider                                           |
| --------------------------- | -------------------------------------------------- |
| `HYPER_API_KEY`             | Charm Hyper                                        |
| `ANTHROPIC_API_KEY`         | Anthropic                                          |
| `OPENAI_API_KEY`            | OpenAI                                             |
| `VERCEL_API_KEY`            | Vercel AI Gateway                                  |
| `GEMINI_API_KEY`            | Google Gemini                                      |
| `SYNTHETIC_API_KEY`         | Synthetic                                          |
| `ZAI_API_KEY`               | Z.ai                                               |
| `MINIMAX_API_KEY`           | MiniMax                                            |
| `HF_TOKEN`                  | Hugging Face Inference                             |
| `CEREBRAS_API_KEY`          | Cerebras                                           |
| `OPENROUTER_API_KEY`        | OpenRouter                                         |
| `IONET_API_KEY`             | io.net                                             |
| `GROQ_API_KEY`              | Groq                                               |
| `AVIAN_API_KEY`             | Avian                                              |
| `OPENCODE_API_KEY`          | OpenCode Zen & Go                                  |
| `VERTEXAI_PROJECT`          | Google Cloud VertexAI (Gemini)                     |
| `VERTEXAI_LOCATION`         | Google Cloud VertexAI (Gemini)                     |
| `AWS_ACCESS_KEY_ID`         | Amazon Bedrock (Claude)                            |
| `AWS_SECRET_ACCESS_KEY`     | Amazon Bedrock (Claude)                            |
| `AWS_REGION`                | Amazon Bedrock (Claude)                            |
| `AWS_PROFILE`               | Amazon Bedrock (Custom Profile)                    |
| `AWS_BEARER_TOKEN_BEDROCK`  | Amazon Bedrock                                     |
| `AZURE_OPENAI_API_ENDPOINT` | Azure OpenAI models                                |
| `AZURE_OPENAI_API_KEY`      | Azure OpenAI models (optional when using Entra ID) |
| `AZURE_OPENAI_API_VERSION`  | Azure OpenAI models                                |

### Subscriptions

If you prefer subscription-based usage, here are some plans that work well in
Crush:

- [Synthetic](https://synthetic.new/pricing)
- [GLM Coding Plan](https://z.ai/subscribe)
- [Kimi Code](https://www.kimi.com/membership/pricing)
- [MiniMax Coding Plan](https://platform.minimax.io/subscribe/coding-plan)

### By the Way

Is there a provider you’d like to see in Crush? Is there an existing model that needs an update?

Crush’s default model listing is managed in [Catwalk](https://github.com/charmbracelet/catwalk), a community-supported, open source repository of Crush-compatible models, and you’re welcome to contribute.

(Upstream's Catwalk badge image removed — see the project at
[charmbracelet/catwalk](https://github.com/charmbracelet/catwalk).)

## Configuration

> [!TIP]
> Crush ships with a builtin `crush-config` skill for configuring itself. In
> many cases you can simply ask Crush to configure itself.

Crush runs great with no configuration. That said, if you do need or want to
customize Crush, configuration can be added either local to the project itself,
or globally, with the following priority:

1. `.crush.json`
2. `crush.json`
3. `$HOME/.config/crush/crush.json`

Configuration itself is stored as a JSON object:

```json
{
  "this-setting": { "this": "that" },
  "that-setting": ["ceci", "cela"]
}
```

As an additional note, Crush also stores ephemeral data, such as application
state, in one additional location:

```bash
# Unix
$HOME/.local/share/crush/crush.json

# Windows
%LOCALAPPDATA%\crush\crush.json
```

> [!TIP]
> You can override the user and data config locations by setting:
>
> - `CRUSH_GLOBAL_CONFIG`
> - `CRUSH_GLOBAL_DATA`

### LSPs

Crush can use LSPs for additional context to help inform its decisions, just
like you would. LSPs can be added manually like so:

```json
{
  "$schema": "https://charm.land/crush.json",
  "lsp": {
    "go": {
      "command": "gopls",
      "env": {
        "GOTOOLCHAIN": "go1.24.5"
      }
    },
    "typescript": {
      "command": "typescript-language-server",
      "args": ["--stdio"]
    },
    "nix": {
      "command": "nil"
    }
  }
}
```

### MCPs

Crush also supports Model Context Protocol (MCP) servers through three transport
types: `stdio` for command-line servers, `http` for HTTP endpoints, and `sse`
for Server-Sent Events.

Shell-style value expansion (`$VAR`, `${VAR:-default}`, `$(command)`, quoting,
nesting) works in `command`, `args`, `env`, `headers`, and `url`, so
file-based secrets work out of the box. You can use values like `"$TOKEN"`
or `"$(cat /path/to/secret/token)"`. Expansion runs through Crush's embedded
shell, so the same syntax works on every supported system, Windows included.

Unset variables expand to the empty string by default, matching bash. For
required credentials, use `${VAR:?message}` so an unset variable fails loudly
at load time with `message` instead of silently resolving to empty:

```json
{ "api_key": "${CODEBERG_TOKEN:?set CODEBERG_TOKEN}" }
```

Headers (both MCP `headers` and provider `extra_headers`) whose value
resolves to the empty string are dropped from the outgoing request rather
than sent as `Header:`. That keeps optional env-gated headers like
`"OpenAI-Organization": "$OPENAI_ORG_ID"` clean when the variable is unset.

Provider `extra_body` is a non-expanding JSON passthrough; put env-driven
values in `extra_headers` or the provider's `api_key` / `base_url`, all of
which do expand.

> **Security note:** `crush.json` is trusted code. Any `$(...)` in it runs at
> load time with your shell's privileges, before the UI appears. Don't launch
> Crush in a directory whose `crush.json` you haven't reviewed.

```json
{
  "$schema": "https://charm.land/crush.json",
  "mcp": {
    "filesystem": {
      "type": "stdio",
      "command": "node",
      "args": ["/path/to/mcp-server.js"],
      "timeout": 120,
      "disabled": false,
      "disabled_tools": ["some-tool-name"],
      "env": {
        "NODE_ENV": "production"
      }
    },
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "timeout": 120,
      "disabled": false,
      "disabled_tools": ["create_issue", "create_pull_request"],
      "headers": {
        "Authorization": "Bearer $GH_PAT"
      }
    },
    "streaming-service": {
      "type": "sse",
      "url": "https://example.com/mcp/sse",
      "timeout": 120,
      "disabled": false,
      "headers": {
        "API-Key": "$(echo $API_KEY)"
      }
    }
  }
}
```

### Hooks

Crush has preliminary support for hooks. For details, see
[the hook guide](./docs/hooks/).

### Ignoring Files

Crush respects `.gitignore` files by default, but you can also create a
`.crushignore` file to specify additional files and directories that Crush
should ignore. This is useful for excluding files that you want in version
control but don't want Crush to consider when providing context.

The `.crushignore` file uses the same syntax as `.gitignore` and can be placed
in the root of your project or in subdirectories.

### Allowing Tools

By default, Crush will ask you for permission before running tool calls. If
you'd like, you can allow tools to be executed without prompting you for
permissions. Use this with care.

```json
{
  "$schema": "https://charm.land/crush.json",
  "permissions": {
    "allowed_tools": [
      "view",
      "ls",
      "grep",
      "edit",
      "mcp_context7_get-library-doc"
    ]
  }
}
```

You can also skip all permission prompts entirely by running Crush with the
`--yolo` flag. Be very, very careful with this feature.

### Disabling Built-In Tools

If you'd like to prevent Crush from using certain built-in tools entirely, you
can disable them via the `options.disabled_tools` list. Disabled tools are
completely hidden from the agent.

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "disabled_tools": ["bash", "sourcegraph"]
  }
}
```

To disable tools from MCP servers, see the [MCP config section](#mcps).

### Disabling Skills

If you'd like to prevent Crush from using certain skills entirely, you can
disable them via the `options.disabled_skills` list. Disabled skills are hidden
from the agent, including builtin skills and skills discovered from disk.

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "disabled_skills": ["crush-config"]
  }
}
```

### Agent Skills

Crush supports the [Agent Skills](https://agentskills.io) open standard for
extending agent capabilities with reusable skill packages. Skills are folders
containing a `SKILL.md` file with instructions that Crush can discover and
activate on demand.

The global paths we looks for skills are:

* `$CRUSH_SKILLS_DIR`
* `$XDG_CONFIG_HOME/agents/skills` or `~/.config/agents/skills/`
* `$XDG_CONFIG_HOME/crush/skills` or `~/.config/crush/skills/`
* `~/.agents/skills/`
* `~/.claude/skills/`
* On Windows, we _also_ look at
  * `%LOCALAPPDATA%\agents\skills\` or `%USERPROFILE%\AppData\Local\agents\skills\`
  * `%LOCALAPPDATA%\crush\skills\` or `%USERPROFILE%\AppData\Local\crush\skills\`
* Additional paths configured via `options.skills_paths`

On top of that, we _also_ load skills in your project from the following
relative paths:

* `.agents/skills`
* `.crush/skills`
* `.claude/skills`
* `.cursor/skills`

```jsonc
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "skills_paths": [
      "~/.config/crush/skills", // Windows: "%LOCALAPPDATA%\\crush\\skills",
      "./project-skills",
    ],
  },
}
```

You can get started with example skills from [anthropics/skills](https://github.com/anthropics/skills):

```bash
# Unix
mkdir -p ~/.config/crush/skills
cd ~/.config/crush/skills
git clone https://github.com/anthropics/skills.git _temp
mv _temp/skills/* . && rm -rf _temp
```

```powershell
# Windows (PowerShell)
mkdir -Force "$env:LOCALAPPDATA\crush\skills"
cd "$env:LOCALAPPDATA\crush\skills"
git clone https://github.com/anthropics/skills.git _temp
mv _temp/skills/* . ; rm -r -force _temp
```

### Desktop notifications

Crush sends desktop notifications when a tool call requires permission and when
the agent finishes its turn. They're only sent when the terminal window isn't
focused _and_ your terminal supports reporting the focus state.

```jsonc
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "disable_notifications": false, // default
  },
}
```

To disable desktop notifications, set `disable_notifications` to `true` in your
configuration. On macOS, notifications currently lack icons due to platform
limitations.

### Initialization

When you initialize a project, Crush analyzes your codebase and creates
a context file that helps it work more effectively in future sessions.
By default, this file is named `AGENTS.md`, but you can customize the
name and location with the `initialize_as` option:

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "initialize_as": "AGENTS.md"
  }
}
```

This is useful if you prefer a different naming convention or want to
place the file in a specific directory (e.g., `CRUSH.md` or
`docs/LLMs.md`). Crush will fill the file with project-specific context
like build commands, code patterns, and conventions it discovered during
initialization.

### Attribution Settings

By default, Crush adds attribution information to Git commits and pull requests
it creates. You can customize this behavior with the `attribution` option:

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "attribution": {
      "trailer_style": "co-authored-by",
      "generated_with": true
    }
  }
}
```

- `trailer_style`: Controls the attribution trailer added to commit messages
  (default: `assisted-by`)
  - `assisted-by`: Adds `Assisted-by: Crush:[ModelID]` as specified in [the convention](https://docs.kernel.org/process/coding-assistants.html#attribution)
  - `co-authored-by`: Adds `Co-Authored-By: Crush <crush@charm.land>`
  - `none`: No attribution trailer
- `generated_with`: When true (default), adds `💘 Generated with Crush` line to
  commit messages and PR descriptions

### Custom Providers

Crush supports custom provider configurations for both OpenAI-compatible and
Anthropic-compatible APIs.

> [!NOTE]
> Note that we support two "types" for OpenAI. Make sure to choose the right one
> to ensure the best experience!
>
> - `openai` should be used when proxying or routing requests through OpenAI.
> - `openai-compat` should be used when using non-OpenAI providers that have OpenAI-compatible APIs.

#### OpenAI-Compatible APIs

Here’s an example configuration for Deepseek, which uses an OpenAI-compatible
API. Don't forget to set `DEEPSEEK_API_KEY` in your environment.

```json
{
  "$schema": "https://charm.land/crush.json",
  "providers": {
    "deepseek": {
      "type": "openai-compat",
      "base_url": "https://api.deepseek.com/v1",
      "api_key": "$DEEPSEEK_API_KEY",
      "models": [
        {
          "id": "deepseek-chat",
          "name": "Deepseek V3",
          "cost_per_1m_in": 0.27,
          "cost_per_1m_out": 1.1,
          "cost_per_1m_in_cached": 0.07,
          "cost_per_1m_out_cached": 1.1,
          "context_window": 64000,
          "default_max_tokens": 5000
        }
      ]
    }
  }
}
```

#### Anthropic-Compatible APIs

Custom Anthropic-compatible providers follow this format:

```json
{
  "$schema": "https://charm.land/crush.json",
  "providers": {
    "custom-anthropic": {
      "type": "anthropic",
      "base_url": "https://api.anthropic.com/v1",
      "api_key": "$ANTHROPIC_API_KEY",
      "extra_headers": {
        "anthropic-version": "2023-06-01"
      },
      "models": [
        {
          "id": "claude-sonnet-4-20250514",
          "name": "Claude Sonnet 4",
          "cost_per_1m_in": 3,
          "cost_per_1m_out": 15,
          "cost_per_1m_in_cached": 3.75,
          "cost_per_1m_out_cached": 0.3,
          "context_window": 200000,
          "default_max_tokens": 50000,
          "can_reason": true,
          "supports_attachments": true
        }
      ]
    }
  }
}
```

### Amazon Bedrock

Crush currently supports running Anthropic models through Bedrock, with caching disabled.

- A Bedrock provider will appear once you have AWS configured, i.e. `aws configure`
- Crush also expects the `AWS_REGION` or `AWS_DEFAULT_REGION` to be set
- To use a specific AWS profile set `AWS_PROFILE` in your environment, i.e. `AWS_PROFILE=myprofile crush`
- Alternatively to `aws configure`, you can also just set `AWS_BEARER_TOKEN_BEDROCK`

### Vertex AI Platform

Vertex AI will appear in the list of available providers when `VERTEXAI_PROJECT` and `VERTEXAI_LOCATION` are set. You will also need to be authenticated:

```bash
gcloud auth application-default login
```

To add specific models to the configuration, configure as such:

```json
{
  "$schema": "https://charm.land/crush.json",
  "providers": {
    "vertexai": {
      "models": [
        {
          "id": "claude-sonnet-4@20250514",
          "name": "VertexAI Sonnet 4",
          "cost_per_1m_in": 3,
          "cost_per_1m_out": 15,
          "cost_per_1m_in_cached": 3.75,
          "cost_per_1m_out_cached": 0.3,
          "context_window": 200000,
          "default_max_tokens": 50000,
          "can_reason": true,
          "supports_attachments": true
        }
      ]
    }
  }
}
```

### Local Models

Local models can also be configured via OpenAI-compatible API. Here are two common examples:

#### Ollama

```json
{
  "providers": {
    "ollama": {
      "name": "Ollama",
      "base_url": "http://localhost:11434/v1/",
      "type": "openai-compat",
      "models": [
        {
          "name": "Qwen 3 30B",
          "id": "qwen3:30b",
          "context_window": 256000,
          "default_max_tokens": 20000
        }
      ]
    }
  }
}
```

#### LM Studio

```json
{
  "providers": {
    "lmstudio": {
      "name": "LM Studio",
      "base_url": "http://localhost:1234/v1/",
      "type": "openai-compat",
      "models": [
        {
          "name": "Qwen 3 30B",
          "id": "qwen/qwen3-30b-a3b-2507",
          "context_window": 256000,
          "default_max_tokens": 20000
        }
      ]
    }
  }
}
```

## Logging

Sometimes you need to look at logs. Luckily, Crush logs all sorts of
stuff. Logs are stored in `./.crush/logs/crush.log` relative to the project.

The CLI also contains some helper commands to make perusing recent logs easier:

```bash
# Print the last 1000 lines
crush logs

# Print the last 500 lines
crush logs --tail 500

# Follow logs in real time
crush logs --follow
```

Want more logging? Run `crush` with the `--debug` flag, or enable it in the
config:

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "debug": true,
    "debug_lsp": true
  }
}
```

## Provider Auto-Updates

By default, Crush automatically checks for the latest and greatest list of
providers and models from [Catwalk](https://github.com/charmbracelet/catwalk),
the open source Crush provider database. This means that when new providers and
models are available, or when model metadata changes, Crush automatically
updates your local configuration.

### Disabling automatic provider updates

For those with restricted internet access, or those who prefer to work in
air-gapped environments, this might not be want you want, and this feature can
be disabled.

To disable automatic provider updates, set `disable_provider_auto_update` into
your `crush.json` config:

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "disable_provider_auto_update": true
  }
}
```

Or set the `CRUSH_DISABLE_PROVIDER_AUTO_UPDATE` environment variable:

```bash
export CRUSH_DISABLE_PROVIDER_AUTO_UPDATE=1
```

### Manually updating providers

Manually updating providers is possible with the `crush update-providers`
command:

```bash
# Update providers remotely from Catwalk.
crush update-providers

# Update providers from a custom Catwalk base URL.
crush update-providers https://example.com/

# Update providers from a local file.
crush update-providers /path/to/local-providers.json

# Reset providers to the embedded version, embedded at crush at build time.
crush update-providers embedded

# For more info:
crush update-providers --help
```

## Metrics

Crush records pseudonymous usage metrics (tied to a device-specific hash),
which maintainers rely on to inform development and support priorities. The
metrics include solely usage metadata; prompts and responses are NEVER
collected.

Details on exactly what’s collected are in the source code ([here](https://github.com/charmbracelet/crush/tree/main/internal/event)
and [here](https://github.com/charmbracelet/crush/blob/main/internal/llm/agent/event.go)).

You can opt out of metrics collection at any time by setting the environment
variable by setting the following in your environment:

```bash
export CRUSH_DISABLE_METRICS=1
```

Or by setting the following in your config:

```json
{
  "options": {
    "disable_metrics": true
  }
}
```

Crush also respects the [`DO_NOT_TRACK`](https://donottrack.sh/) convention
which can be enabled via `export DO_NOT_TRACK=1`.

## Q&A

### Why is clipboard copy and paste not working?

Installing an extra tool might be needed on Unix-like environments.

| Environment         | Tool                     |
| ------------------- | ------------------------ |
| Windows             | Native support           |
| macOS               | Native support           |
| Linux/BSD + Wayland | `wl-copy` and `wl-paste` |
| Linux/BSD + X11     | `xclip` or `xsel`        |

## Contributing

See the [contributing guide](https://github.com/charmbracelet/crush?tab=contributing-ov-file#contributing).

## Whatcha think?

We’d love to hear your thoughts on this project. Need help? We gotchu. You can find us on:

- [Twitter](https://twitter.com/charmcli)
- [Slack][slack]
- [Discord][discord]
- [The Fediverse](https://mastodon.social/@charmcli)
- [Bluesky](https://bsky.app/profile/charm.land)

[slack]: https://charm.land/slack
[discord]: https://charm.land/discord

## License

[FSL-1.1-MIT](https://github.com/charmbracelet/crush/raw/main/LICENSE.md)

---

Upstream is part of [Charm](https://charm.land). This fork is an
independent project living under the same FSL-1.1-MIT license but
with no affiliation to Charm Industries beyond the shared upstream
codebase.

<!--prettier-ignore-->
Charm热爱开源 • Charm loves open source
