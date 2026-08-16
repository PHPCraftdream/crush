# CLI-provider model refresh + reasoning-effort correctness

Status: **research done / implementation not started**
Date: 2026-08-16
Scope: `internal/agent/cliprovider/provider.go`, `internal/cmd/models_atoms.go`,
`internal/cmd/ping.go`

## Method

None of the four CLIs can enumerate their own models (`--help` documents a
free-form `--model/-m` string; there is no `models list` subcommand). Model
data below was extracted from the **installed packages themselves**, not from
memory:

- `codex` → an **embedded JSON model registry** inside
  `@openai/codex/node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe`
  (authoritative: carries `context_window`, `supported_reasoning_levels`,
  `default_reasoning_level`, `upgrade` pointers)
- `gemini` → `VALID_GEMINI_MODELS` set in
  `@google/gemini-cli/bundle/chunk-QCOIICKD.js`
- `claude` → string table of `@anthropic-ai/claude-code/bin/claude.exe`
- `qwen` → uses the Claude wire format; not separately enumerated

Installed versions: `claude` 2.1.197, `codex-cli` 0.147.0, `gemini` 0.55.1,
`qwen` 0.15.11.

---

## 0. P1 BUG FOUND: `--effort` is injected into CLIs that reject it

`cliprovider/provider.go:943-957` applies the session's reasoning effort by
appending `--effort <level>` to **whatever** CLI is being launched:

```go
if effort, ok := ctx.Value(ReasoningEffortContextKey).(string); ok && effort != "" {
    // ...replace an existing --effort, else:
    args = append(args, "--effort", effort)
}
```

Only `claude` has that flag. Verified by running the real binaries:

```
$ codex exec --effort high --help
error: unexpected argument '--effort' found
```

`codex` takes reasoning effort as `-c model_reasoning_effort=<level>`
(confirmed: `model_reasoning_effort = "low"` appears in its config schema).
`gemini` and `qwen` have no effort flag at all.

**Reachability.** `agent.go:1916` sets the context value unconditionally from
`currentSession.LargeModelReasoningEffort` for every session, with no
per-provider guard. `LargeModelReasoningEffort` is a persisted session column,
so the realistic path is: operator sets an effort while the session is on a
Claude model → switches the same session to a codex model → every subsequent
turn dies instantly with an unknown-flag error. `ping.go:293-294` has the same
shape.

**Fix**: effort application must become a per-spec concern —
`CLISpec.ApplyEffort func(args []string, effort string) []string` (nil = this
CLI has no effort knob, silently ignore), replacing the hardcoded `--effort`
splice. Claude keeps the flag form; codex gets
`-c model_reasoning_effort=<level>`; gemini/qwen get nil.

This is independent of, and more urgent than, the model-list refresh below.

---

## 1. Codex — authoritative registry (the big surprise)

Parsed from the embedded registry in `codex.exe` (codex-cli 0.147.0):

| slug | ctx | default effort | supported efforts | upgrade→ |
|---|---|---|---|---|
| `gpt-5.6-sol` | 272 000 | low | low, medium, high, xhigh, max, **ultra** | — |
| `gpt-5.6-terra` | 272 000 | medium | low, medium, high, xhigh, max, **ultra** | — |
| `gpt-5.6-luna` | 272 000 | medium | low, medium, high, xhigh, max | — |
| `gpt-5.5` | 272 000 | medium | low, medium, high, xhigh | — |
| `gpt-5.4` | 272 000 | medium | low, medium, high, xhigh | `gpt-5.6-terra` |
| `gpt-5.4-mini` | 272 000 | medium | low, medium, high, xhigh | `gpt-5.6-luna` |
| `gpt-5.2` | 272 000 | medium | low, medium, high, xhigh | — |
| `codex-auto-review` | 272 000 | medium | low, medium, high, xhigh | internal |

`display_name`s are `GPT-5.6-Sol` / `-Terra` / `-Luna`. Descriptions:
Sol = "Latest frontier agentic coding model"; Terra = balanced quality/latency/
cost; Luna = replacement for 5.4-mini.

**Two corrections to the earlier draft of this document:**

- There is **no `gpt-5.6-pro`**. It was an artifact of a sloppy regex over the
  binary's string table (adjacent strings concatenate, e.g.
  `gpt-5.6-solopenai.gpt-5.6-solGPT-5.6 Sol`). The operator was right to
  challenge it. Plain `gpt-5.6` exists only as a *family alias* in a doc table
  ("verify its currently documented routing and availability"), not as a
  registry slug.
- Codenames `sol`/`terra`/`luna` are **not** internal experiments — they are
  the real, current, top-priority slugs (`priority` 1/2/3).

**Our current `All` is badly stale.** Four of our six codex entries are *not
in the registry at all*:

| our ModelID | our model arg | in registry? |
|---|---|---|
| `cli-codex` | `gpt-5.3-codex` | **NO** |
| `cli-codex-gpt-5-2` | `gpt-5.2-codex` | **NO** |
| `cli-codex-max` | `gpt-5.1-codex-max` | **NO** |
| `cli-codex-mini` | `gpt-5.1-codex-mini` | **NO** |
| `cli-codex-gpt-5-4` | `gpt-5.4` | yes (deprecated → terra) |
| `cli-codex-gpt-5-2-base` | `gpt-5.2` | yes |

Also every codex spec declares `ContextWindow: 400_000` while the registry
says **272 000**. `ContextWindow` drives auto-summarization thresholds, so a
48% overstatement means we let conversations run well past the real limit.

---

## 2. Claude

Aliases the CLI accepts: `default`, `opus`, `opusplan`, `sonnet`, `haiku`,
`fable`, `mythos`, plus 1M-context forms `opus[1m]`, `opusplan[1m]`,
`sonnet[1m]`, `fable[1m]`.

Documented effort levels (`claude --help`): **low, medium, high, xhigh, max**.

| Model ID | In our `All`? | Note |
|---|---|---|
| `claude-sonnet-5` | **no** | Claude-5 family |
| `claude-mythos-5` | **no** | whole family absent; `mythos` alias exists |
| `claude-fable-5` | yes (via `fable` alias) | |
| `claude-opus-4-8` | yes | **newest Opus** |
| `claude-opus-4-7`, `claude-opus-4-6` | yes | |
| `claude-opus-4-7-fast`, `claude-opus-4-6-fast` | no | "fast" variants |
| `claude-haiku-4-5` | yes (via `haiku` alias) | |

### RETRACTED CLAIM: "there is no `claude-opus-5`"

An earlier revision of this document asserted that `claude-opus-5` does not
exist, on the grounds that the literal `opus-5` occurs zero times in
`claude.exe`'s string table. **That conclusion was wrong**, and the reasoning
behind it was invalid: the CLI passes an unrecognised `--model` value straight
through to the API, so it has no reason to embed every model ID it can serve.
Absence from the string table is not evidence of absence.

Verified by actually running it:

```
$ claude --model claude-opus-5 -p "hi" --output-format json
"modelUsage": {"claude-opus-5": {"contextWindow": 200000, "maxOutputTokens": 32000}}

$ claude --model "claude-opus-5[1m]" -p "hi" --output-format json
"modelUsage": {"claude-opus-5[1m]": {"contextWindow": 1000000, "maxOutputTokens": 32000}}
```

`claude-opus-5` is real, and the `[1m]` suffix is a real context-window
switch (200k → 1M), not cosmetic. **Empirical pinging is the only reliable
method for the Claude family**; only codex ships a genuinely authoritative
embedded registry.

**Stale label bug.** `cli-claude-sonnet` is displayed as
`"Claude Sonnet 4.6 (CLI)"` but bound to the *moving* `sonnet` alias, which
the CLI resolves to its own current default — now plausibly Sonnet 5. Same
drift risk on `cli-claude-opus` ("latest"). Either pin the ID or drop the
version number from the display name; do not keep both.

---

## 3. Gemini — authoritative list

`VALID_GEMINI_MODELS`:

| Model ID | Constant | In our `All`? |
|---|---|---|
| `gemini-3-pro-preview` | `PREVIEW_GEMINI_MODEL` | no |
| `gemini-3.1-pro-preview` | `PREVIEW_GEMINI_3_1_MODEL` | **yes** (`cli-gemini-pro`) |
| `gemini-3.1-pro-preview-customtools` | custom-tools variant | no |
| `gemini-3-flash-preview` | `PREVIEW_GEMINI_FLASH_MODEL` | no |
| `gemini-2.5-pro` | `DEFAULT_GEMINI_MODEL` | no |
| `gemini-2.5-flash` | `DEFAULT_GEMINI_FLASH_MODEL` | no |
| **`gemini-3.5-flash`** | `DEFAULT_GEMINI_3_5_FLASH_MODEL` | **no — newest flash** |
| `gemini-3-flash` | `SECONDARY_GEMINI_3_5_FLASH_MODEL` | **yes** (`cli-gemini-flash`) |
| **`gemini-3.1-flash-lite`** | `DEFAULT_GEMINI_FLASH_LITE_MODEL` | **no** |
| `gemma-4-31b-it`, `gemma-4-26b-a4b-it` | Gemma local routing | no |

Aliases: `auto`, `pro`, `flash`, `flash-lite`. No reasoning-effort flag.

Note our `cli-gemini-flash` pins `gemini-3-flash`, which the CLI itself now
labels *secondary* to `gemini-3.5-flash`.

---

## 4. `crush ping` and effort

`--model` **already** accepts an `@effort` suffix — `parseAtomOrRaw`
(`models_atoms.go:562-601`) splits it and `validateEffortForModel`
(`:613-631`) checks it against the atom's `Levels()`. So
`crush ping --model local-cli/cli-codex-sol@xhigh` is already the intended
syntax; what is missing is:

1. **It does nothing useful for codex today** — see §0; the effort would be
   spliced in as `--effort`, which codex rejects.
2. **Validation is atom-gated.** `validateEffortForModel` returns `nil` when
   the provider/model has no registered atom (`key == ""`) or the atom has no
   `Levels()`. Any new CLI model without an atom therefore accepts *any*
   effort string silently. New specs need matching atoms (or a spec-level
   effort list) to get validation.
3. **`ultra`** is a new codex-only level not present anywhere in our effort
   vocabulary today (`low|medium|high|xhigh|max`).

Recommendation: keep `@effort` as the syntax (no new flag — a separate
`--effort` flag would need mutual-exclusion rules with `--role`, and `@` is
already documented in the flag help), and additionally source the valid levels
per CLI model from the spec rather than only from `atomRegistry`.

---

## 5. Implementation order

1. **§0 effort-dispatch fix** (`CLISpec.ApplyEffort`) — standalone bug, ship
   first, with a test per CLI asserting the produced argv.
2. Correct codex `ContextWindow` 400 000 → 272 000 on existing entries.
3. Refresh the spec list: add `gpt-5.6-sol/terra/luna`, `gpt-5.5`,
   `claude-sonnet-5`, `claude-mythos-5`, `gemini-3.5-flash`,
   `gemini-3.1-flash-lite`; retire the four codex slugs missing from the
   registry.
4. Fix the stale `cli-claude-sonnet` / `cli-claude-opus` display names.
5. Add `ultra` to the effort vocabulary, gated to models that declare it.
6. Matching atoms in `models_atoms.go` for anything the operator wants a short
   code for.
7. **Ping every added model** (`crush ping --model local-cli/<id>[@effort]`)
   and record the result table here. Retire anything that fails.

## 5a. Ping matrix — measured, not assumed (2026-08-16)

Every row below was executed against the real CLI on this machine.

### Claude (`claude` 2.1.197) — all OK

| argument passed | resolves to | ctx | max out |
|---|---|---|---|
| `claude-opus-5` | `claude-opus-5` | 200 000 | 32 000 |
| `claude-opus-5[1m]` | `claude-opus-5[1m]` | **1 000 000** | 32 000 |
| `claude-sonnet-5` | `claude-sonnet-5` | 1 000 000 | 64 000 |
| `claude-mythos-5` | `claude-mythos-5` | 1 000 000 | 64 000 |
| `claude-opus-4-8` | `claude-opus-4-8` | 1 000 000 | 64 000 |
| `claude-fable-5` | `claude-fable-5` | 1 000 000 | 64 000 |
| alias `opus` | `claude-opus-4-8` | 1 000 000 | 64 000 |
| alias `sonnet` | **`claude-sonnet-5`** | 1 000 000 | 64 000 |
| alias `haiku` | `claude-haiku-4-5-20251001` | 200 000 | 32 000 |
| alias `fable` | `claude-fable-5` | 1 000 000 | 64 000 |
| alias `fable[1m]` | `claude-fable-5` (no change) | 1 000 000 | 64 000 |

**Stale-label bug now proven, not inferred**: our `cli-claude-sonnet` passes
the alias `sonnet` and is displayed as *"Claude Sonnet 4.6 (CLI)"*, but the
alias actually resolves to **Sonnet 5**. The UI has been naming the wrong
model.

Note `opus` resolves to 4.8, **not** to Opus 5 — so Opus 5 is unreachable
through any alias and needs its own explicit spec entries (both the 200k and
the `[1m]` form).

### Codex (`codex-cli` 0.147.0)

Invocation needs `--skip-git-repo-check` outside a trusted repo.

| model | result |
|---|---|
| `gpt-5.6-sol` | OK (`turn.completed`) |
| `gpt-5.6-terra` | OK |
| `gpt-5.6-luna` | OK |
| `gpt-5.5` | OK |
| `gpt-5.3-codex` | **`Model metadata for 'gpt-5.3-codex' not found. Defaulting to fallback metadata; this can degrade performance`** |
| `gpt-5.1-codex-max` | same error |

Confirms the registry: our four legacy codex slugs are unsupported. They do
not hard-fail — they silently run on fallback metadata with degraded
performance, which is arguably worse than failing.

### Gemini (`gemini` 0.55.1)

Needs `--skip-trust`; the JSON body is preceded by a Windows-10 warning line
that must be stripped before parsing.

| model | result |
|---|---|
| `gemini-3.5-flash` | OK |
| `gemini-3.1-flash-lite` | OK |
| `gemini-3-flash` | OK, but **the response reports the model as `gemini-3.5-flash`** — it is now a redirect |
| `gemini-3.1-pro-preview` | inconclusive — `You exceeded your current quota` (account limit, not a model-existence failure); retest later |

Because `gemini-3-flash` silently redirects, our `cli-gemini-flash` entry is
already running 3.5 while advertising 3.

**Bonus finding (feeds task #469):** gemini's result event carries
`"cached": 8148` and `"input": 4453` alongside `"input_tokens": 12601` —
cache data we currently discard. See the cache-stats plan.

## 6. Open questions

- Retire the four dead codex entries outright, or keep them as hidden aliases
  so existing DB rows / atoms referencing them do not dangle? (The claude spec
  list already uses the "keep the alias entry so old rows don't dangle"
  pattern — worth mirroring.)
- Expose `gemini-2.5-pro` / `gemini-3-pro-preview` / the Gemma entries at all,
  or keep the gemini list to the two newest?
- `claude-mythos-5`: expose as its own spec, and does it need different
  effort levels than the rest of the Claude family?
