---
description: Register a local sub-agent to fall back to when `crush` becomes unusable — peak-hours refusal or hard quota limit — switch to it automatically the moment either fires, and switch back on user request or once the peak-hours window closes
---

`/crush-fallback <agent>` **arms** a fallback: it names the local `Agent` sub-agent type (e.g. `sh`, `oh`, `fm`) to use *if and when* `crush` becomes unusable. It does **NOT** switch anything right now.

**Keep using `/crush` (`crush run`) as normal.** Switch to `<agent>` only when one of exactly two things happens:

1. **Peak hours** — a `crush run` refused because the provider is inside its configured peak-hours window.
2. **Hard limit** — weekly/monthly quota exhausted, account suspended, or "context window exceeded"/"quota exceeded" with no retry-after landing inside this session.

Nothing else counts. A slow run, a transient 5xx, a `Process restarted`, your own `--timeout` firing, a confusing error — all of those mean **re-run `crush run` against the same `--session`**, not switch.

When one of the two does fire: **switch immediately and silently.** Don't ask the operator for permission, don't ask them for the refusal text, don't confirm — arming this command already was their instruction. Just route the work through `Agent({subagent_type: "<agent>"})` and say one line about it.

And once you have switched, **stay switched until something else tells
you the window is over** — the cron for peak-hours, `/crush-fallback
clear` for a hard limit. Re-attempting `crush run` in the meantime is
the single most common way this command gets misused; see "Once armed,
stop checking" below for what that costs and what to do instead.

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

The one string that always appears is `is in peak hours (` in the failed
run's stderr / `--json` `.error`. Match on that. Don't infer the refusal
from `exit_reason` alone — `cancelled`/`error` are shared with timeouts,
max-cost and generic failures.

There are **two** shapes, and you must handle both:

- **Full guidance block** — `is in peak hours (` plus `RESUME AT:`
  (a local date-time and an RFC3339 stamp) plus `peak-hours window`.
  This is `internal/agent/peak_hours_stop.go`'s `PeakHoursGuidance`,
  emitted when the top-level run is refused.
- **Short form** — only `is in peak hours (HH:MM–HH:MM), refusing until
  HH:MM`, with no `RESUME AT:` and no `peak-hours window`. This comes
  from a different path: the refusal surfacing as `failed to start agent
  processing stream: …` when a **sub-agent's** stream is what hit the
  window. Observed in practice; a run can do 800+ seconds of real work
  and only then be refused this way.

Requiring all three markers would make you miss the short form entirely
and keep hammering a provider that has already said no.

Then:

1. Get the reopen moment.
   - Full block: take the RFC3339 stamp from `RESUME AT:` verbatim —
     it is already exact, day-wrap handled.
   - Short form: build it from `refusing until HH:MM`, interpreted in
     **local** time. If that time-of-day is not later than now, it means
     tomorrow.
   If the resulting moment is already in the past, the window closed
   while you were reading: stay on `/crush`, don't switch.
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

### Once armed, stop checking — the cron IS the resume signal

While `STATUS: active`, **`/crush` does not exist for you.** Not as a
first choice, not as a fallback-from-the-fallback, not "just to see".

Concretely, until the cron's `# crush-fallback resume` prompt actually
fires, do NOT:

- launch `crush run` for any reason, including a task that feels
  different, smaller, or more urgent than the one that got refused;
- compare the current time against `UNTIL` and conclude the window has
  reopened — the cron owns that decision, and it fires on its own;
- re-attempt a refused run "to check" whether the provider is back;
- poll `crush sessions why` / `sessions locks` / `sessions list` hoping
  for a different answer;
- rewrite `UNTIL` or `CRON_JOB_ID`, or arm a second cron.

A manual retry before the real reopen does not get you an early start.
It gets you the same refusal, minus the tokens and minutes the run burnt
before hitting it — and, on the sub-agent path, that can be a *lot*: the
refusal may arrive only after the run has already done substantial work,
leaving partial edits in the tree that you then have to triage.

Between arming and the cron firing there is exactly one correct
behaviour: **keep routing new delegated work through `<agent>`, and
otherwise leave the marker alone.**

The same applies with `TRIGGER: hard-limit`, where there is no cron at
all: nothing you can observe will end that state. Only
`/crush-fallback clear` does.

The liveness watchdog `/crush` prescribes (`crush sessions locks` every
~10 min) is for **`crush run` processes you launched**. It does not
apply to `<agent>` runs and must not be repurposed into peak-hours
polling — the harness notifies you when a sub-agent finishes.

## `/crush-fallback clear`

Ends fallback now. If the marker already reads `dormant`, say so and stop. Otherwise: `CronDelete` the `CRON_JOB_ID` if one is recorded, set the marker to `STATUS: dormant`, report back. Any `<agent>` run already in flight finishes on its own — `clear` only stops NEW delegation through it.

## Task

$ARGUMENTS
