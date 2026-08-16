# Unified per-message token usage & cache statistics

Status: **CLI feature complete (stats, periods, help, docs); web UI display and
run-envelope surfacing still open**
Date: 2026-08-16
Scope: `zai` (HTTP, via fantasy) + `claude-cli` / `codex-cli` / `gemini-cli`
(local CLI, via `internal/agent/cliprovider`)

Goal: record cache-hit and token-usage statistics **per assistant message**,
for **every** provider, through **one** canonical type and **one** mapping
layer — instead of today's four divergent, partly-lossy ad-hoc paths.

---

## 1. What exists today (verified, with file:line)

### 1.1 The canonical-ish type is `fantasy.Usage`

`charm.land/fantasy@v0.25.2/model.go:11-28`

```go
type Usage struct {
    InputTokens         int64
    OutputTokens        int64
    TotalTokens         int64
    ReasoningTokens     int64
    CacheCreationTokens int64
    CacheReadTokens     int64
}
```

It is the transport for all providers. It is a third-party struct — we cannot
add fields to it, only decide what we put in it and what we map it onto.

### 1.2 Four providers, **two mutually incompatible conventions**

This is the core defect. `InputTokens` means different things depending on who
filled it in, and nothing in the code records which convention was used.

| Provider | `InputTokens` | `CacheCreationTokens` | `CacheReadTokens` | Source |
|---|---|---|---|---|
| **zai** (openai-compatible via fantasy) | **EXCLUSIVE**: `prompt_tokens − cached_tokens` | `0` (no cache-write concept in the OpenAI-compat API) | `prompt_tokens_details.cached_tokens` | `fantasy/providers/openai/language_model_hooks.go:218-225, 244-253` |
| **claude-cli** | **INCLUSIVE**: `input + cache_creation + cache_read` | **`0` — dropped** | **`0` — dropped** | `cliprovider/provider.go:347-379` |
| **codex-cli** | **INCLUSIVE**: `input + cached_input` | `0` | **`0` — dropped** | `cliprovider/provider.go:461-477` |
| **gemini-cli** | **INCLUSIVE**: reads `stats.input_tokens` | `0` | **`0` — dropped** | `cliprovider/provider.go:387-399` |

> **Correction (empirical, 2026-08-16).** An earlier draft of this document
> claimed gemini-cli "emits no cache data at all". That was read off our own
> `geminiCLIEvent` struct, not off the CLI. Verified against the real
> `gemini` 0.55.1 `--output-format stream-json` result event:
>
> ```json
> {"type":"result","status":"success","stats":{
>   "total_tokens":12898, "input_tokens":12601, "output_tokens":1,
>   "cached":8148, "input":4453, "duration_ms":25076, "tool_calls":0}}
> ```
>
> Gemini **does** report cache reads (`cached`) and an exclusive input count
> (`input`), and `input_tokens == input + cached` (12601 = 4453 + 8148) — i.e.
> the same INCLUSIVE convention as claude/codex. Our struct simply never
> declares the `cached`/`input` fields, so the data is dropped at unmarshal.
> **Consequence: all four providers can report cache reads**, so
> `CacheSupport: none` may end up unused — but keep the enum, since a future
> provider or an older CLI build can still be silent.

Every downstream consumer assumes the **EXCLUSIVE** convention:

```go
// internal/agent/agent.go:4544
if promptTokens := usage.InputTokens + usage.CacheReadTokens; promptTokens != 0 {
    session.PromptTokens = promptTokens
}

// internal/agent/agent.go:4517-4520
cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
    modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
    modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
    modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)
```

(`CostPer1MInCached` = cache **write** price, `CostPer1MOutCached` = cache
**read** price — confusingly named upstream in `catwalk/provider.go:88-89`,
but used correctly here.)

**Today the totals still come out right** — for CLI providers the cache tokens
are folded into `InputTokens` *and* the cache fields are zero, so the sum is
accidentally correct. Two consequences:

1. **The breakdown is unrecoverable.** No cache-hit statistic can be computed
   downstream, for any CLI provider, at any point after the parser returns.
2. **It is a live trap.** Anyone who "fixes" a CLI parser to populate
   `CacheReadTokens` *without simultaneously* switching `InputTokens` to
   exclusive will silently **double-count** prompt tokens — inflating
   `session.PromptTokens`, triggering premature auto-summarization, and
   double-billing on any model that has non-zero costs. This is the single
   most important invariant the refactor must lock down with a test.

### 1.3 Cost impact is currently nil for CLI providers — but only by accident

`internal/config/load.go:564-592` synthesizes the `local-cli` provider's model
list with **only** `ID / Name / ContextWindow / DefaultMaxTokens`; every
`CostPer1M*` field defaults to `0`. Line 589 (`provider.Models = models`)
unconditionally overwrites the list on every load, so a user cannot supply
costs via `crush.json` either. Net effect: CLI-provider cost is always `0`,
so the dropped cache breakdown is **not** currently a billing bug. It is a
*statistics* bug — and a latent billing bug the moment anyone adds prices.

Same for the fork's synthesized `glm-5.3` entry (`load.go:535-547`) — no cost
fields. Other zai models come from catwalk with real prices.

### 1.4 The statistic is computed and then thrown away

`cliprovider/provider.go:360-374` — claude-cli already computes exactly the
number we want and logs it:

```go
cacheHitPct = float64(ev.Usage.CacheReadInputTokens) / float64(inputTotal) * 100
slog.Info("cliprovider: token usage", ..., "cache_hit_pct", ...)
```

It never leaves the log line. codex-cli's `cached_input_tokens`
(`provider.go:469`) is summed into the input total and likewise discarded.

### 1.5 Nothing is persisted per message

- `message.Finish` (`internal/message/content.go:123-136`) carries
  `Reason / Time / Message / Details / Partial` — **no usage**.
- The `messages` table (`internal/db/migrations/20250424200609_initial.sql:47-57`)
  has **no token columns**: `id, session_id, role, parts, model, created_at,
  updated_at, finished_at` (+ later fork columns).
- Only `sessions.prompt_tokens / completion_tokens / cost` exist, and the token
  ones are **last-snapshot-overwrite**, not cumulative
  (`agent.go:4540-4547`).
- `PartWire` (`internal/server/wire.go:7-32`) has no usage fields, so the web
  UI could not display it even if we stored it.

### 1.6 Ordering constraint in `agent.go`

`AddFinish` is called at `agent.go:2822 / 2838 / 2840`, **before** usage is
resolved at `2859-2860`. So a `Finish`-embedded usage field would require
either reordering the (delicate, heavily-commented) finish chain or a second
write pass. **The design below sidesteps this entirely** — see §2.3.

---

## 2. Design

### 2.1 One canonical type — `message.TokenUsage`

New file `internal/message/usage.go`. This is the only shape anything
downstream of a provider is allowed to speak.

```go
// TokenUsage is the canonical, provider-independent token accounting for one
// assistant message.
//
// CONVENTION (load-bearing — see the CLI parsers and TestUsageConvention):
// InputTokens counts ONLY tokens that were billed as fresh input. Tokens
// served from the provider's prompt cache are in CacheReadTokens; tokens
// written INTO the cache are in CacheCreationTokens. The three are disjoint.
// Total prompt size is InputTokens + CacheReadTokens + CacheCreationTokens.
//
// Providers that report the INCLUSIVE convention (a single input count with
// cache folded in) must be normalized at their parser boundary, never here.
type TokenUsage struct {
    InputTokens         int64 `json:"input_tokens,omitempty"`
    OutputTokens        int64 `json:"output_tokens,omitempty"`
    ReasoningTokens     int64 `json:"reasoning_tokens,omitempty"`
    CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
    CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`

    // CostUSD is the cost delta this single message contributed, computed
    // with the same four-tier formula as updateSessionUsage. Zero for
    // flat-rate and for CLI providers (which carry no prices at all).
    CostUSD float64 `json:"cost_usd,omitempty"`

    // Provider/Model record WHICH model produced this message — a session
    // can switch models mid-conversation, and sub-agent messages can use a
    // different model than their parent.
    Provider string `json:"provider,omitempty"`
    Model    string `json:"model,omitempty"`

    // CacheSupport says whether the cache numbers above are meaningful.
    // Without it a Gemini-CLI message is indistinguishable from a genuine
    // 0% cache hit, which would be a lie in a stats view.
    CacheSupport CacheSupport `json:"cache_support,omitempty"`

    // Estimated is true when the provider omitted usage entirely and
    // fallbackStepUsage synthesized these numbers from message lengths
    // (internal/agent/usage_fallback.go:18-34). Such rows must be excluded
    // from, or flagged in, any aggregate.
    Estimated bool `json:"estimated,omitempty"`
}

type CacheSupport string

const (
    // CacheSupportNative — provider reports a real cache breakdown.
    // zai, claude-cli, codex-cli (read only).
    CacheSupportNative CacheSupport = "native"
    // CacheSupportNone — provider is silent about caching; the zero values
    // in CacheReadTokens/CacheCreationTokens mean "unknown", NOT "no hits".
    // gemini-cli.
    CacheSupportNone CacheSupport = "none"
)
```

Helper methods (`CacheHitRatio() (float64, bool)`, `PromptTokens() int64`,
`IsZero() bool`) live next to it so no caller re-derives the arithmetic.
`CacheHitRatio` returns `ok=false` when `CacheSupport != native` — callers
must render "n/a", not "0%".

### 2.2 One normalization layer per side

**CLI side** — new `internal/agent/cliprovider/usage.go`. All three parsers
stop building `fantasy.Usage` by hand and funnel through one function:

```go
// rawUsage is what a CLI reported, in ITS OWN convention.
type rawUsage struct {
    input         int64 // may or may not include cache, see inputIncludesCache
    output        int64
    cacheCreation int64
    cacheRead     int64
    inputIncludesCache bool
    support       CacheSupport
}

// normalize converts any provider's raw counts into the canonical EXCLUSIVE
// convention. This is THE ONLY place the inclusive->exclusive subtraction
// happens.
func (r rawUsage) normalize() fantasy.Usage
```

- `claudeParseUsageLine` → `inputIncludesCache: false`, `support: native`.
  **Confirmed empirically** against `claude` 2.1.197: a real result event
  returned `input_tokens: 5842`, `cache_creation_input_tokens: 16984`,
  `cache_read_input_tokens: 0` — the three are disjoint, so Claude's
  `input_tokens` is already exclusive and today's code *adds* the cache
  counters onto it. That addition is exactly the fold we are removing.
- `codexParseUsageLine` → codex's `input_tokens` **includes**
  `cached_input_tokens`, so `inputIncludesCache: true` and the normalizer
  subtracts; `cacheRead: cached_input_tokens`, `support: native`.
- `geminiParseUsageLine` → `inputIncludesCache: true` (`stats.input_tokens`
  includes `stats.cached`), `cacheRead: stats.cached`, `support: native`.
  The `geminiCLIEvent` struct must gain the `cached` and `input` fields —
  they exist in the CLI's output today and are silently discarded.

> **Verify before implementing.** The claude-vs-codex inclusive/exclusive
> split above is read off the current parser code and the CLIs' JSON shapes;
> it must be confirmed against a real recorded `result` / `turn.completed`
> line from each CLI (capture one via `slog` debug, assert
> `input + cache_create + cache_read == <the CLI's own total>`), because
> getting it backwards silently double- or half-counts. Ship a golden-file
> test per CLI with a captured real line.

`CacheSupport` is also stored on `CLISpec` (`UsageCacheSupport` field) so a
spec without a `ParseUsageLine` is classified honestly rather than defaulting
to "native, zero hits".

**HTTP side** — new `internal/agent/usage_normalize.go`:

```go
// usageFromFantasy maps a fantasy.Usage (already exclusive by contract) plus
// the resolved Model onto the canonical type, attaching provenance and the
// cost delta the caller just computed.
func usageFromFantasy(u fantasy.Usage, model Model, costDelta float64, estimated bool) message.TokenUsage
```

Cache-support classification for HTTP providers is derived from the fantasy
provider type (anthropic / openai-compatible / google → `native`; anything
whose hook never sets the cache fields → `none`). Keep this in one small
table with a comment naming the fantasy source file each entry was read from,
so a fantasy upgrade that changes behaviour is a one-line diff here.

### 2.3 Storage: dedicated columns on `messages`, not the `parts` blob

Two candidate stores were considered:

- **(a) inside the `Finish` part's JSON** — no migration, but forces the
  `AddFinish`-before-usage reordering described in §1.6, and makes
  aggregation (`sessions cost`, a future `sessions cache`) parse every
  message's JSON blob.
- **(b) columns on `messages`** — one cheap migration, real SQL aggregation,
  **and no reordering**: usage is written by a separate `SetUsage` call placed
  exactly where the cost delta is already computed.

**Choose (b).** New migration `..._add_usage_to_messages.sql`:

```sql
ALTER TABLE messages ADD COLUMN input_tokens INTEGER;
ALTER TABLE messages ADD COLUMN output_tokens INTEGER;
ALTER TABLE messages ADD COLUMN reasoning_tokens INTEGER;
ALTER TABLE messages ADD COLUMN cache_creation_tokens INTEGER;
ALTER TABLE messages ADD COLUMN cache_read_tokens INTEGER;
ALTER TABLE messages ADD COLUMN cost_usd REAL;
ALTER TABLE messages ADD COLUMN usage_provider TEXT;
ALTER TABLE messages ADD COLUMN usage_model TEXT;
ALTER TABLE messages ADD COLUMN cache_support TEXT;
ALTER TABLE messages ADD COLUMN usage_estimated INTEGER;
```

Nullable, no defaults — NULL means "this message predates the feature" and is
distinguishable from a real zero. `message.Message` gains `Usage *TokenUsage`
(pointer, so "never recorded" ≠ "recorded as all-zero").

Follow the `UpdateSessionModels` precedent from the model-cascade work
(`internal/db/sql/sessions.sql`): use `sqlc.arg('id')` for the WHERE clause,
never a bare `?` mixed with numbered placeholders — SQLite continues numbering
a bare `?` from the highest explicit index, which breaks positional binding.

New query `UpdateMessageUsage` + `message.Service.SetUsage(ctx, messageID,
TokenUsage) error`.

### 2.4 Wiring point in `agent.go`

Exactly one new call, next to the two that already persist usage:

```go
// internal/agent/agent.go, around 2859-2868 — unchanged lines elided
usage, estimated := fallbackStepUsage(stepMessages, stepResult)
costDelta := a.updateSessionUsage(largeModel, &updatedSession, usage, a.openrouterCost(...))
if costDelta != 0 { /* IncrementCost — unchanged */ }
if err := a.sessions.SetUsage(ctx, updatedSession.ID, ...); err != nil { ... }

// NEW: per-message usage, same data, message granularity.
if err := a.messages.SetUsage(ctx, currentAssistant.ID,
    usageFromFantasy(usage, largeModel, costDelta, estimated)); err != nil {
    // non-fatal: statistics must never fail a turn
    slog.Warn("agent: failed to record per-message usage", "err", err)
}
```

Note `fallbackStepUsage` already returns the `estimated` bool at
`usage_fallback.go:18` — today it is discarded at the call site
(`agent.go:2859` uses `usage, _ :=`). Reuse it instead of adding a new signal.

The same three-line block goes into the two summarization paths
(`agent.go:3793`, `agent.go:3974`), which already call `updateSessionUsage`.

**Non-fatal by design**: a statistics write must never abort or fail a turn.

### 2.5 Sub-agents

`coordinator.go` transfers a finished child's cost to the parent. Per-message
rows are written in the **child's own session** (that is where its messages
live); the parent's delegation message keeps recording only what the parent's
own model consumed. Aggregating a whole delegation tree is then a join over
`sessions.parent_session_id`, not a special case in the writer. Add an
explicit test that a sub-agent run does **not** attribute child tokens to the
parent's message row.

### 2.6 Read paths

1. **Wire/UI** — `MessageWire` (`internal/server/wire.go:35-49`) gains a
   `Usage *UsageWire`; `web/src/types.ts` mirrors it; the message component
   renders `↓ 12.3k · ↑ 840 · cache 87%` (and `cache n/a` when
   `CacheSupport != "native"`), matching however cost is displayed today.
2. **CLI** — extend `crush sessions cost` with cache columns, and add
   `crush sessions cache [<id>]` for a per-session / per-model cache-hit
   breakdown. Both become real `SUM()` queries thanks to §2.3.
3. **`crush run --json`** — add a `usage` object to the envelope so an
   orchestrator can read cache efficiency without opening the DB.

---

## 3. Task breakdown

| # | Task | Status |
|---|---|---|
| 1 | `message.TokenUsage` + `CacheSupport` + helpers + tests | **DONE** |
| 2 | Capture a real usage line per CLI and settle the inclusive-vs-exclusive question | **DONE** - see section 1.2; all three measured by running the binaries repeatedly against a fixed prompt |
| 3 | `cliprovider/usage.go` normalizer; all 3 parsers ported; golden tests on captured lines | **DONE** |
| 4 | DB migration + `UpdateMessageUsage` + `SetUsage` + aggregate queries | **DONE** |
| 5 | Wire into `agent.go` (main turn + both summarization paths) | **DONE** |
| 6 | Sub-agent attribution test (child tokens stay in the child session) | **OPEN** |
| 7 | `MessageWire.Usage` + `web/src/types.ts` + per-message display | **OPEN** |
| 8 | `sessions cache` command, incl. `--since` / `--by model|day` across sessions | **DONE** |
| 8b | `sessions cost` cache columns | **DONE (as a documented pointer, not merged columns)** - `sessions cost` reads session-level last-snapshot counters while the cache view reads per-message rows. Putting both in one table would imply they are comparable, so its help now states the caveat and directs to `sessions cache` instead. |
| 9 | `usage` object in the `crush run --json` envelope | **OPEN** |
| 10 | CHANGELOG + README + command help | **DONE** |

### Bugs found and fixed while implementing

Three, none of which were in the original plan as *existing* defects:

1. **codex double-counted cached tokens.** Its `input_tokens` already contains
   `cached_input_tokens`; the parser summed them. A captured turn reported
   23768 where the true prompt was 16856 - a 41% overstatement that inflated
   `session.PromptTokens` and pulled auto-summarization forward.
2. **gemini's cache data was discarded at unmarshal.** `geminiCLIEvent` never
   declared the `cached`/`input` fields the CLI actually emits, which is why
   the first draft of this document wrongly recorded gemini as reporting no
   cache at all.
3. **`PromptTokens` omitted cache-WRITE tokens.** `updateSessionTokenCounters`
   summed `InputTokens + CacheReadTokens` only. Because the three classes are
   disjoint, this understated the prompt for every provider reporting them
   separately - the Anthropic HTTP provider always did, and claude-cli joined
   it the moment its own fold was removed. A measured turn (input 5842,
   cache_creation 16984) recorded 5842 instead of 22826: a 74% understatement,
   and in the dangerous direction, since it delays compaction and risks
   overrunning the context window rather than merely wasting a summarize.

Item 3 was introduced *by this work* and caught only because the operator
asked "is the bug fixed?", prompting an end-to-end recheck rather than a
re-read of the diff. Worth remembering as an argument for tracing the whole
path after a convention change, not just the code that was edited.

## 4. Explicit non-goals / honest gaps

- ~~**gemini-cli will report `cache_support: none`**~~ - **retracted.** That
  claim was read off our own struct, not the CLI. gemini does report `cached`
  and an exclusive `input`; it is now classified `native` like the rest. The
  `CacheSupportNone` path is retained anyway, because an older CLI build or a
  future provider can still be silent, and a fabricated 0% must never stand in
  for "unknown".
- **codex-cli reports cache reads only.** Its `turn.completed` usage has
  `cached_input_tokens` but no cache-creation counter
  (`provider.go:436-441`), so `CacheCreationTokens` stays 0 for codex — which
  is a genuine absence, not a dropped value.
- **No retroactive backfill.** Messages written before the migration keep
  `NULL` usage forever; aggregates must count and report how many rows were
  skipped rather than silently treating them as zero.
- **CLI-provider costs stay 0.** Adding real prices to `local-cli` models is
  out of scope (and arguably wrong — those runs are billed against the
  operator's CLI subscription, not per-token). The plumbing will nonetheless
  compute cost correctly the day prices appear.
