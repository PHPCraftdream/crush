# `--root` flag + reviewer-consult path — deferred to post-release

## Origin

Raised by the user mid-session (2026-08-03): should `crush run` gain a
`--root` flag telling the top-level agent it is the most senior agent in
the chain — nobody to ask — so it must resolve every decision itself, or
by consulting the `reviewer` model slot if one is configured?

Deliberately **not implemented before this release**. This file exists so
the idea isn't lost, not to commit to a design.

## Current state (as of this session)

The "nobody to ask" half is already mostly true in practice, just not
explicit:

- `isSessionAgent.isSubAgent` (`internal/agent/agent.go:332`,
  `coordinator.go:1293`) already distinguishes top-level from delegated
  runs internally — it's a derived value, not a CLI flag or something the
  model is told about directly.
- The coder system prompt (`internal/agent/templates/coder.md.tpl`, rule
  2) already pushes hard on autonomy: *"Be autonomous — search, read,
  decide, act, don't ask what you can find out... Stop only for real
  external blocks... or a genuinely ambiguous high-stakes decision."*
- `crush run --help` already states "non-interactive by design (no human
  at the keyboard)".
- BUT: `ask_question` is unconditionally added to the toolset
  (`coordinator.go:1435`), including for the root `crush run` invocation.
  If the model calls it, the run ends cleanly with
  `exit_reason: "awaiting_answer"` (`app.go:482`) and a resume command —
  not a crash, but a burned run for a genuinely unattended invocation
  (cron, CI, an orchestrator that won't come back to answer). There is
  currently no way to suppress this — `--restrict-run`/`--allow-tool`
  gate *permission requests*, and `ask_question` doesn't go through the
  permission system at all.

The "consult the reviewer" half **does not exist at all**. `reviewer` is
today only a model *slot* (`crush models use --reviewer`), reachable only
by an operator explicitly running `crush run --role reviewer`. There is no
in-run path for the root agent to consult it mid-turn.

## Why deferred

1. The last five `@oh` review rounds this session (tasks #213-253) were
   all about bugs in the delegation/watchdog/cost-transfer machinery —
   every round found something, including the fifth. A reviewer-consult
   path opens exactly that same surface again (new tool, second
   model-invocation branch, cost accounting, interaction with the
   parent's watchdog), with zero review coverage yet.
2. No user-facing bug exists today. `ask_question` at the root gives a
   clean exit + resume command, not a hang or crash — suboptimal, not
   broken.
3. Even the cheap half touches the system prompt, which regressed once
   already this session before ever shipping (see CHANGELOG, task #171:
   "Orchestrator-mode prompt regression fixed before it ever shipped").

## Suggested shape for a future task (not a commitment)

Split into two independent, separately-reviewable pieces:

### (a) `--root` flag — cheap, mostly plumbing
- New `crush run --root` bool flag (default false, or maybe should
  default true for `crush run` specifically since it's *already*
  non-interactive by definition — worth deciding explicitly, not
  assuming).
- When set: either strip `ask_question` from the toolset entirely, or
  keep it but add a system-prompt nudge that calling it here is a last
  resort with no one to answer — needs a real decision, not just "add a
  flag".
- If `reviewer` is configured, `--root` could also nudge the prompt to
  reach for it instead of stopping — but see (b), that requires the tool
  to exist first.

### (b) Reviewer-consult tool — a real feature, needs its own design pass
- A new tool (e.g. `consult_reviewer`) that runs a single, cost-tracked,
  watchdog-bounded call against the `reviewer` slot mid-turn.
- Needs: its own cost-transfer path (same class of bug the parent/child
  delegation cost-transfer already had, see CHANGELOG's "cancelled turn
  could permanently lose a sub-agent's spend"), its own timeout
  relationship with the parent's watchdog (same class of race as the
  parent/child cleanup-grace bug), and a decision on whether it's
  available only when `--root` is set or generally.
- Should go through the same zero-trust review cycle the rest of this
  session's fixes did before shipping — not bundled into a single PR
  with (a).

## Status

Not filed as a TaskList task yet — this document is the parking spot.
File as a real task (or two, per the split above) when picked up for a
future release.
