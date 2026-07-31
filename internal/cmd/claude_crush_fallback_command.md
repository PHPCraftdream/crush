---
description: Register a local sub-agent to fall back to when `crush` becomes unusable — peak-hours refusal or hard quota limit — switch to it automatically the moment either fires, and switch back on user request or once the peak-hours window closes
---

`/crush-fallback <agent>` **arms** a fallback: it names the local `Agent` sub-agent type (e.g. `sh`, `oh`, `fm`) to use *if and when* `crush` becomes unusable. It does **NOT** switch anything right now.

**Keep using `/crush` (`crush run`) as normal.** Switch to `<agent>` only when one of exactly two things happens:

1. **Peak hours** — a `crush run` refused because the provider is inside its configured peak-hours window.
2. **Hard limit** — weekly/monthly quota exhausted, account suspended, or "context window exceeded"/"quota exceeded" with no retry-after landing inside this session.

Nothing else counts. A slow run, a transient 5xx, a `Process restarted`, your own `--timeout` firing, a confusing error — all of those mean **re-run `crush run` against the same `--session`**, not switch.

When one of the two does fire: **switch immediately and silently.** Don't ask the operator for permission, don't ask them for the refusal text, don't confirm — arming this command already was their instruction. Just route the work through `Agent({subagent_type: "<agent>"})` and say one line about it.

If `<agent>` is missing on invocation, **stop and ask** which agent type to use — that's the only thing you may never pick yourself.

## The eternal marker task

All fallback state lives in ONE TaskList task per session. Find it by
subject **prefix** (never exact-match — the suffix is a live status
summary, see below):

```
crush-fallback state (persistent — do not complete)
```

`TaskCreate` it (`status: pending`) the first time this command runs;
afterwards `TaskUpdate` both its subject and description in place —
never create a second one, never complete or delete it, it's a
sentinel, not work. Keep the subject one line, always starting with
the exact prefix above followed by `: ` and a short summary a human
skimming `TaskList` can read without opening the task, e.g.:

```
crush-fallback state (persistent — do not complete): sh — armed, switches on peak-hours or hard-limit
crush-fallback state (persistent — do not complete): sh — ACTIVE (hard-limit), switch back with /crush-fallback clear
crush-fallback state (persistent — do not complete): dormant
```

It survives `/checkpoint` + `/resume` and context compaction, so a
fresh session sees the fallback state on `TaskList` without depending
on conversation context.

The description is the state:

```
STATUS: armed          # agent registered, /crush still in use — the normal resting state
AGENT: <agent>
```

```
STATUS: active         # a trigger fired; delegated work now goes through <agent>
AGENT: <agent>
TRIGGER: peak-hours | hard-limit
PROVIDER: <id>
UNTIL: <RFC3339>       # peak-hours only; "none" for hard-limit
CRON_JOB_ID: <id>      # peak-hours only; "none" for hard-limit
```

```
STATUS: dormant        # cleared
```

This is instruction-level, not a code hook — `/crush` does not read the marker automatically. Honoring it is on the operating agent.

## On invocation — arm it, then carry on

Upsert the marker to `STATUS: armed` with `<agent>`. Report one line: `fallback armed: <agent> (engages on peak-hours refusal or hard limit; /crush in use until then)`. **Then continue with `/crush` exactly as before** — nothing else changes yet.

Re-invoking with a different agent (armed or active) just replaces `AGENT` in the marker; the trigger state and any existing cron stay as they are.

## Trigger 1 — peak-hours refusal

Recognise it by all three strings in the failed run's stderr / `--json` `.error`:

- `is in peak hours (`
- `RESUME AT:` — followed by a local date-time and an RFC3339 stamp
- `peak-hours window`

(That's the stable block `crush` always emits for this refusal — `internal/agent/peak_hours_stop.go`'s `PeakHoursGuidance`. Don't infer it from `exit_reason` alone; `cancelled`/`error` are shared with timeouts, max-cost and generic failures.)

Then:

1. Take the RFC3339 stamp from `RESUME AT:` verbatim — it's already the exact reopen moment, day-wrap handled. If it's already in the past, the window closed while you were reading: stay on `/crush`, don't switch.
2. Marker → `STATUS: active`, `TRIGGER: peak-hours`, provider id, `UNTIL`.
3. Arm the one-shot auto-revert cron below, record its id into `CRON_JOB_ID`.
4. Route subsequent delegated work through `<agent>`.

```
CronCreate({
  cron: "<MM> <HH> * * *",     // minute+hour of ReopensAt, local time
  recurring: false,
  durable: true,
  prompt: "# crush-fallback resume\n\nThe peak-hours window for provider <id> has closed (was due <RFC3339>).\n\n1. Do NOT interrupt any <agent> run still in flight — let it finish.\n2. Stop launching NEW work through <agent>.\n3. Route subsequent delegated work back through `/crush` (crush run).\n4. TaskUpdate the `crush-fallback state (persistent — do not complete)` task's description back to `STATUS: armed` + `AGENT: <agent>` — do NOT complete or delete the task.\n5. Tell the user fallback has ended and `/crush` is back in use."
})
```

`durable: true` deliberately deviates from `CronCreate`'s default guidance: a multi-hour wait that died with the process would strand the session in fallback with no way back.

## Trigger 2 — hard limit

Weekly/monthly budget exhausted, account suspended, or "context window exceeded"/"quota exceeded" with no retry-after inside this session. No reopen time exists, so: marker → `STATUS: active`, `TRIGGER: hard-limit`, `UNTIL: none`, `CRON_JOB_ID: none`, no cron armed. Route work through `<agent>`. Only `/crush-fallback clear` ends it.

## While active

Hand anything you'd have given `crush run` to `Agent({subagent_type: "<agent>", ...})`, briefed the same way — goal, file-set, definition of done. All of `/crush`'s delegation hygiene still applies: scope call-outs for concurrent work, no parallel git-writing sub-agents over one tree, tests scoped to what changed, zero-trust verification of the diff afterward. This changes the **transport**, not the verification bar.

One agent type for everything — the reason is "crush is unavailable", not task complexity.

## `/crush-fallback clear`

Ends fallback now. If the marker already reads `dormant`, say so and stop. Otherwise: `CronDelete` the `CRON_JOB_ID` if one is recorded, set the marker to `STATUS: dormant`, report back. Any `<agent>` run already in flight finishes on its own — `clear` only stops NEW delegation through it.

## Task

$ARGUMENTS
