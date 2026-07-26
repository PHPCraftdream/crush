---
description: While a crush provider is inside its configured peak-hours refusal window, route delegated work through a local sub-agent instead of `crush run`, and auto-revert to `/crush` the instant the window closes
---

This skill is **opt-in and reactive**, not a standing watcher. Invoke it right after `/crush` (or a bare `crush run`) has just been refused for peak hours: `/crush-fallback <agent>`, where `<agent>` is the exact suffix-agent type from this session's `Agent` tool list (e.g. `sh`, `oh`, `fm`). **Do not invoke pre-emptively** "just in case" a window is about to open — wait for the actual refusal; the resume time this command schedules is read off that refusal, not guessed.

If `<agent>` is missing, **stop and ask** which agent type to use — never default to one on your own initiative (same rule as `--allow-peak-hours`: no silent choices on the operator's behalf).

`/crush-fallback clear` (no agent argument) ends fallback mode early, manually — see "Clearing early" below.

## The eternal marker task

All fallback state lives in ONE TaskList task per session, created the first time `/crush-fallback` runs and never marked `completed` for the rest of the session — it is a permanent sentinel, not a piece of work to finish. Look for a task whose subject is exactly:

```
crush-fallback state (persistent — do not complete)
```

If it doesn't exist yet, `TaskCreate` it with that exact subject and `status: "pending"` (pending, not in_progress — nothing is "in progress" about a sentinel). If it already exists, `TaskUpdate` its `description` in place — never create a second one, never complete or delete it as part of normal operation (the one exception is `clear`, which still doesn't delete it — it just resets the description to dormant).

The description field is the actual state, written as plain structured text:

```
STATUS: active
AGENT: <agent>
PROVIDER: <id>
UNTIL: <RFC3339 ReopensAt>
CRON_JOB_ID: <id returned by CronCreate>
```

or, when dormant:

```
STATUS: dormant
```

Because this task is never completed, it survives `/checkpoint` + `/resume` and context compaction exactly like every other open task — a fresh session picking this repo back up will see it on `TaskList` and know immediately whether fallback is (or was) active, without depending on anything staying in conversation context. This is intentionally NOT a technical interception — `/crush` itself does not read this marker automatically. Routing decisions are a matter of the operating agent honoring this instruction, exactly like the rest of `/crush`'s guidance is instruction-level, not a code hook.

## Step 1 — Confirm this is a peak-hours refusal, not something else

Do not enter fallback mode on a bare non-zero exit or a generic error. Look at the refusal text (stderr of the `crush run` that just failed, or what the user pasted) for **all** of:

- `is in peak hours (` — the provider/window/refusing-until fragment.
- `RESUME AT:` — followed by a local date-time and an RFC3339 stamp.
- the phrase `peak-hours window` in the surrounding guidance paragraph.

These three strings are the stable signature `crush` always emits for this specific refusal (pre-flight refusal at turn start, and mid-turn forced stop, both print the identical guidance block — see `internal/agent/peak_hours_stop.go`'s `PeakHoursGuidance`, documented as the single source of truth for this text). If you only have `--json` output, do not infer peak-hours from `exit_reason` alone — `cancelled`/`error` are shared with timeouts, max-cost, max-tokens and generic failures; confirm via the same three strings in the accompanying stderr/`.warnings`/error text.

If none of these are present, this is **not** the peak-hours case — go to the existing "Fallback when `crush` hits rate limits" flow instead, or just retry per the normal transient-failure rules. Do not run both fallbacks at once.

## Step 2 — Extract the exact reopen time, and bail out if it's already past

Take the RFC3339 timestamp from the `RESUME AT:` line directly — it is already the precise moment the provider reopens, computed by `crush` itself (`PeakHoursError.ReopensAt`), including correct day-wrap handling for overnight windows. Do not recompute it from the `HH:MM-HH:MM` window yourself. If that line is truly unavailable, fall back to `crush providers show <id>` for the configured window and compute the next end time by hand — but prefer `RESUME AT:` whenever present.

**Before touching the eternal task or scheduling anything**, compare the extracted time to now. If it is already in the past — the window closed while you were reading/acting on the refusal — there is nothing to fall back from: tell the operator the window has already reopened and go straight back to `/crush`, without creating/updating the eternal task and without arming a cron.

## Step 3 — Enter fallback mode

Upsert the eternal task (see above) to `STATUS: active` with the current `<agent>`, provider id, and `UNTIL` timestamp (fill in `CRON_JOB_ID` after step 4 returns it).

From this point on, until the eternal task reads `STATUS: dormant` again: any task you would normally hand to `/crush` (`crush run ...`), hand instead to `Agent({ subagent_type: "<agent>", ... })`, briefed exactly as you would brief `crush` — goal, file-set, definition of done. The same delegation hygiene from `/crush`'s "Launching" section still applies (scope call-outs for anything concurrent, no git-writing sub-agents running in parallel over the same tree, tests scoped to what changed, zero-trust verification of the diff afterward). This command changes the **transport**, not the verification bar.

Unlike the rate-limit fallback (`@ao46l` for complex / `@ash` for simple), this fallback uses **one single agent type** — whatever the operator passed as `<agent>` — for everything, because the underlying reason has nothing to do with task complexity; it is purely "this provider is closed for the next N hours."

## Step 4 — Arm a one-shot alarm for the window's close

Use `CronCreate` with `recurring: false` — fires exactly once at the next matching wall-clock time, then auto-deletes. Peak-hours windows are `HH:MM-HH:MM` with no date component, so a 5-field cron built from just the end time's hour/minute (with `*` for day-of-month, month, day-of-week) fires at the correct next occurrence:

```
CronCreate({
  cron: "<MM> <HH> * * *",     // minute, hour of ReopensAt, local time
  recurring: false,
  durable: true,
  prompt: "# crush-fallback resume\n\nThe peak-hours window for provider <id> has closed (was due <RFC3339>).\n\nDo the following now:\n1. Do NOT interrupt any <agent> run that is still in flight — let it finish.\n2. Stop launching any NEW delegated work through <agent>.\n3. Route all subsequent delegated work back through `/crush` (crush run), as normal.\n4. TaskUpdate the `crush-fallback state (persistent — do not complete)` task's description to `STATUS: dormant` — do NOT complete or delete the task itself.\n5. Tell the user fallback mode has ended and `/crush` is back in use."
})
```

`durable: true` is a deliberate deviation from `CronCreate`'s own default guidance (which reserves `durable` for explicit user requests) — a multi-hour wait that dies with the process would silently strand the session in fallback mode with no way back to `/crush` if Claude Code restarts mid-window. Record the returned job id into the eternal task's `CRON_JOB_ID` field immediately — needed by both `clear` and re-invocation (step 5).

## Step 5 — Re-invocation while already active

`/crush-fallback <agent2>` called while the eternal task reads `STATUS: active` SILENTLY SUPERSEDES the previous fallback, no confirmation needed: `CronDelete` the old `CRON_JOB_ID`, re-run Steps 1-4 for the new refusal/agent, and upsert the SAME eternal task (never a second one) with the new agent, provider, until-time, and cron job id. One-line report: "fallback re-armed: was <agent1> until <T1>, now <agent2> until <T2>".

## Clearing early — `/crush-fallback clear`

Ends fallback manually, before the alarm would have fired — e.g. the operator wants `/crush` back immediately regardless of the window. If the eternal task already reads `STATUS: dormant`, there's nothing to clear — say so and stop. Otherwise: `CronDelete` the `CRON_JOB_ID` from the eternal task's description, `TaskUpdate` the eternal task's description to `STATUS: dormant`, and report back. Same non-interruption rule as the alarm: any `<agent>` run already in flight finishes on its own — `clear` only stops NEW delegation through it.

## Do not confuse with "Fallback when `crush` hits rate limits"

That section (in `/crush`'s own body) fires on a **hard, non-recovering** condition — weekly/monthly budget exhausted, suspended account, context window exceeded — with no known return time, so it re-routes immediately and stays re-routed indefinitely (no alarm, no auto-revert).

This command fires on a **soft, self-clearing** condition — a daily local-time window that reopens on a known schedule — so it re-routes temporarily and auto-reverts via the cron alarm. Never treat a peak-hours refusal as grounds for the rate-limit fallback's two-tier `@ao46l`/`@ash` routing, and never treat a hard-limit refusal as grounds for scheduling a `CronCreate` resume — there is no reopen time to wait for.

## Task

$ARGUMENTS
