# CLI-provider model refresh + reasoning-effort correctness

Status: **Claude additions shipped (commit d9a394f6); codex/gemini refresh and
the effort-dispatch fix still open**
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

## 0. P1 BUG: `--effort` is injected into CLIs that reject it

`cliprovider/provider.go:943-957` applies the session's reasoning effort by
appending `--effort <level>` to **whatever** CLI is being launched:

```go
if effort, ok := ctx.Value(ReasoningEffortContextKey).(string); ok && effort != "" {
    // ...replace an existing --effort, else:
    args = append(args, "--effort", effort)
}
```

Only `claude` has that flag.

### Blast radius: 3 of 4 CLI families, 9 of 19 specs

An earlier revision of this document scoped the bug to codex. That was too
narrow. Verified by running each real binary:

```
$ codex exec --effort high --help
error: unexpected argument '--effort' found

$ gemini --skip-trust --effort high -m gemini-3.5-flash -p "hi"
Unknown argument: effort

$ qwen --effort high -p "hi"
Unknown argument: effort
```

Every non-claude spec in `All` breaks the moment a session carries an effort:
gemini (2 specs), qwen (1), codex (6) - 9 of the 19 currently registered.
Only the 10 claude specs are safe. Note qwen is easy to miss: it reuses
`claudePartParser`/`claudeParseUsageLine` and so *looks* Claude-shaped, but it
is a different binary with a different flag set.

### Codex does support effort - via a different mechanism

`codex` takes it as `-c model_reasoning_effort=<level>`. Confirmed working end
to end (`turn.completed`) for both `high` and `ultra`.

An invalid value is rejected by the API, which helpfully enumerates the set:

```
[ReasoningEffortParam] [reasoning.effort] [invalid_enum_value]
Invalid value: 'bogus'. Supported values are:
'none', 'minimal', 'low', 'medium', 'high', 'xhigh', and 'max'.
```

Note `ultra` is absent from that API list yet succeeds through the CLI, and
codex's embedded registry lists `ultra` only for `gpt-5.6-sol`/`-terra` - so
the CLI is translating it client-side. Treat the **registry** as the source of
truth for which levels a given codex model accepts.

### Why the fix needs TWO pieces, not one

Levels are per-MODEL, not per-CLI (from the embedded registry, section 1):

| model | accepted efforts |
|---|---|
| `gpt-5.6-sol`, `gpt-5.6-terra` | low, medium, high, xhigh, max, ultra |
| `gpt-5.6-luna` | low, medium, high, xhigh, max |
| `gpt-5.5`, `gpt-5.4`, `gpt-5.2` | low, medium, high, xhigh |
| claude family | low, medium, high, xhigh, max |

So merely switching codex to the correct `-c` form is NOT sufficient: a
session carrying `max` from a Claude model would then reach `gpt-5.5` in a
*valid-looking* flag and fail with a 400 at the API instead of at argv parse.
The fix therefore needs both:

- `CLISpec.ApplyEffort func(args []string, effort string) []string` - HOW this
  CLI receives an effort. nil means "no effort knob"; gemini/qwen get nil.
- `CLISpec.EffortLevels []string` - WHICH values this model accepts. An effort
  outside the set is skipped and logged rather than passed through.

### Reachability

`agent.go:1916` sets the context value unconditionally from
`currentSession.LargeModelReasoningEffort`, with no per-provider guard;
`ping.go:293-294` has the same shape.

The web UI *does* gate the effort picker
(`ModelSelector.tsx: showEffortPicker = isCLIClaudeModel || isZAIReasoningModel`),
so an effort cannot be dialled onto a codex/gemini/qwen model directly. But it
does NOT clear a stored effort when the session moves to a model without one -
the clamping `useEffect` bails out first:

```ts
if (!session || !showEffortPicker) return;
```

`LargeModelReasoningEffort` is a persisted session column, so the live path is:
set an effort on a Claude model, switch that same session to codex/gemini/qwen,
and every subsequent turn dies. Clearing the stale value is a genuine part of
the fix (see task #473's note - the same trap must not be rebuilt in the
Default-models modal, where a bad value written at *global* scope would be
inherited by every future session).

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

| # | Work | Status |
|---|---|---|
| 1 | **Effort dispatch fix** (section 0): `CLISpec.ApplyEffort` + `CLISpec.EffortLevels`, plus clearing a stale session effort when the model has no effort knob. Test asserting the produced argv per CLI family. | **open - task #471, do first** |
| 2 | Correct codex `ContextWindow` 400 000 -> 272 000 on existing entries | open - task #470 |
| 3 | Add `gpt-5.6-sol` / `-terra` / `-luna`, `gpt-5.5` (all ping OK) | open - task #470 |
| 4 | Decide retire-vs-alias for the four codex slugs missing from the registry | open - task #470 |
| 5 | Add `gemini-3.5-flash`, `gemini-3.1-flash-lite` (both ping OK); relabel `cli-gemini-flash`, which silently runs 3.5 | open - task #470 |
| 6 | Claude 5 pinned entries + stale alias labels | **DONE - commit d9a394f6** |
| 7 | Matching atoms for anything needing a short code | partly done (opus5/sonnet5/fable5 added) |
| 8 | Web UI effort control in the Default-models modal | open - tasks #472 -> #473 |

Item 1 gates items 3 and 5 in practice: adding more non-claude models widens
the blast radius of the effort bug before it is fixed.

Verification bar: each behavioural fix gets a revert-check (reintroduce the
bug, confirm the new test fails, restore, confirm the diff is unchanged), and
every new model is pinged before being added - `crush ping --model
local-cli/<id>[@effort]`, or the CLI directly.

## 6. Open questions

- Retire the four dead codex entries outright, or keep them as hidden aliases
  so existing DB rows / atoms referencing them do not dangle? They do not
  hard-fail today - they warn `Model metadata not found. Defaulting to
  fallback metadata; this can degrade performance` and run anyway, which is
  arguably worse than failing. The claude spec list already uses the
  "keep the alias so old rows don't dangle" pattern, worth mirroring.
- Expose `gemini-2.5-pro` / the Gemma entries at all, or keep the gemini list
  to the newest two?
- `claude-mythos-5` is confirmed working (ping OK, 1M ctx) but is not yet
  exposed as a spec. Add it, and does it want different effort levels than the
  rest of the Claude family?
- Once #471 lands and codex accepts an effort, should the Default-models modal
  and `ModelSelector` offer it for codex models? That needs the per-model level
  sets from the registry to reach the frontend rather than being hardcoded a
  third time (see task #472).

## 7. Resolved since the first draft

- ~~"there is no `claude-opus-5`"~~ - **wrong**, retracted in section 2. It
  exists (200k; `[1m]` form gives 1M). Absence from a binary's string table is
  not evidence of absence when the CLI forwards unknown model ids to the API.
- ~~"`gpt-5.6-pro`"~~ - **does not exist**, was a regex artifact (section 1).
- ~~"the bug is codex-only"~~ - it is codex + gemini + qwen (section 0).
