---
description: Delegate this task to a crush sub-agent instead of doing it yourself
---

This skill is **opt-in only** — invoked by typing `/crush <task>` (or
"delegate this to crush"). Build a `crush run` and launch it within
this single invocation. **Do NOT auto-invoke on later turns**: if the
user didn't type `/crush` again, they chose to handle the follow-up
with you directly.

## When NOT to delegate (even after /crush)

Refuse, say so in one sentence, and offer to do it directly:

- **Interactive by nature** — merges, conflict resolution, design or
  debugging needing back-and-forth. A sub-agent cannot stop and ask.
- **Depends on this chat's context** — files just read, decisions made
  earlier, partial state. A sub-agent starts cold.
- **The user gave the answer** — "fix it like this" wants *your* hands
  so you can verify the assumption.
- **One- or two-line work** — delegation overhead > the change.
- **Mid-plan with the user** — keep the plan coherent in one head.

## Fallback when `crush` hits rate limits

Fall back to a local `Agent` sub-agent **only** on a hard window/quota
limit that won't recover by retrying — weekly/monthly budget gone,
account suspended, or "context window exceeded"/"quota exceeded" with
no retry-after that lands inside this session. Re-route immediately,
without asking:

- Complex / non-trivial work → `@ao46l` (agent) — Opus, heavier model.
- Simple / one-liner / mechanical task → `@ash` (agent) — Sonnet, faster.

Brief the sub-agent as you would `crush`: goal, file-set it may touch,
what "done" looks like. The zero-trust verification rule below still
applies — verify the diff and re-run the tests yourself.

**Do NOT fall back** for transient/recoverable failures — re-run
instead:

- your own `--timeout` fired — re-run `crush run` against the **same
  `--session` id** with a larger `--timeout`.
- situational HTTP 429 with short retry-after — wait it out, re-run.
- 5xx / network blip — re-run; escalate to the user if it persists.
- operator-side errors (bad flag, missing workspace, malformed prompt)
  — fix the invocation, retry.

The local-agent fallback is the **last resort**, not a shortcut around
transient failures.

## Never self-add `--allow-peak-hours`

A provider can refuse to run during a local-time window (`crush
providers set <id> --peak-hours HH:MM-HH:MM`, overnight wrap allowed,
`--peak-hours off` clears it). `crush run --allow-peak-hours` bypasses
that refusal for one invocation only — no persistent config-level
equivalent exists.

**Never add `--allow-peak-hours` on your own initiative.** Only pass it
when a human operator has explicitly asked, in this specific request,
to override peak hours for that task. If a run refuses this way and
the user hasn't authorized a bypass, report the refusal and the
retry-after time and wait for their decision — do not route around it
by adding the flag, switching provider, or re-running until the window
closes, unsolicited.

## Launching

- `--role smart` for non-trivial, `--role fast` for one-liners.
- Stable, task-meaningful `--session` id (issue/branch/topic slug) —
  same id continues across runs and is recognisable in `sessions watch`.
- `--timeout 60m` as the standard ceiling, on every run. It's generous
  on purpose: a mid-edit timeout leaves partial state, so a long
  ceiling is cheap insurance (the run still ends once the task is
  done). Drop lower only for a genuinely tiny task where you want a
  fast failure signal.
- Run in the background (`Bash` `run_in_background: true`), redirect
  to `.crush/stdin/<task>.{out,err}`, react on the completion
  notification. Don't sleep-poll for output — do run the liveness
  watchdog below.
- **Never background the process yourself with a trailing `&`.** The
  Bash call must be the bare `crush run ...` command, passed with
  `run_in_background: true` on the tool call — nothing else. `&` (plus
  anything after it, e.g. `echo`) makes Bash track the *wrapper shell*,
  which exits instantly on detach — so "completed" fires before
  `crush run` finishes. That false-done signal is what causes a second
  `crush run` into the same still-locked `--session` (`already in use`
  / lock races / crashed sessions). One command per Bash call, no `&`:

  ```
  # Correct
  Bash({ command: "crush run --role smart --session foo --timeout 60m --json < .crush/stdin/foo.prompt > .crush/stdin/foo.out 2> .crush/stdin/foo.err", run_in_background: true })

  # Wrong — false-completes instantly, invites session-id reuse before the real process exits
  Bash({ command: "crush run ... > out 2> err &\necho launched pid $!", run_in_background: true })
  ```

- Multi-line prompts → `Write` to `.crush/stdin/<task>.prompt`, feed
  via `< file`. Avoid positional `"…"` past one line.
- Permissions inside `crush run` are auto-approved — run only in
  workspaces you can afford to lose.
- **Default to one `crush run` in flight at a time per worktree.**
  Sequential is the safe default — launch, wait for the real
  completion notification, verify, then launch the next. Only run more
  than one concurrently when the task genuinely decomposes into
  independent file-sets AND the user asked for parallelism (or the
  task explicitly calls for a fan-out).
- **Parallel runs** MUST name the file-set each prompt may touch
  ("only edit `internal/foo/`") — two runs writing the same file race
  and corrupt silently.
- **Parallel runs MUST warn each agent about the others** — name the
  sibling runs and their scopes in every prompt, e.g. "You are one of
  N agents working concurrently on this repo. Others: `run-b` (scope:
  `internal/server/`), `run-c` (scope: `web/src/`). Stay strictly
  inside your own scope; don't touch files outside it even if
  something looks related." An agent unaware it's sharing the tree
  will "helpfully" touch a file it saw referenced elsewhere.
- **Parallel runs MUST forbid git writes** — no `commit`, `add`,
  `stash`, `reset`, `checkout`/`restore`, `rebase`, `merge` in any
  prompt; concurrent index/tree writes clobber each other
  (`index.lock` races, one run's `checkout` reverting another's
  edits). Read-only git (`status`/`diff`/`log`) is fine. The
  orchestrator stages and commits **sequentially, itself**, after all
  runs finish and each diff is verified. When edits genuinely overlap,
  give each run its own `git worktree`.
- **Parallel runs MUST run tests isolated to their own scope, or not
  at all** — each prompt runs tests only for its assigned package(s)
  (e.g. `go test ./internal/foo/...`, never `./...`); concurrent test
  runs across a module can race on shared build cache/fixtures. If a
  run can't scope its tests narrowly, it skips them and says so in its
  summary — the orchestrator runs the shared suite once, after all
  runs land and are merged.

## Monitoring

`crush sessions watch` is the primary monitor — auto-detects end and
prints a summary (duration, tokens, cost); unlike `sessions tail
--follow` it never hangs on a dead lock.

```
crush sessions watch              # interactive picker → live-tail
crush sessions watch <id>         # live-tail directly (short hash ok)
```

Live-tail shows tool calls with their key arguments inline:

```
[tool: bash] go test ./internal/agent/...
[tool: edit] internal/cmd/sessions.go
[tool-result: edit] internal/cmd/sessions.go: <result> (+3 lines)
```

Read-only secondaries: `sessions list` (STATUS column), `sessions
locks` (heartbeat: `alive`/`ping`/`stopping`/`offline`), `sessions show
<id> --with-messages`, `sessions last <id> [--n N]`, and `sessions why
<id>` (plain-language explanation of why a session has the status it
has — reach for this before manually cross-referencing `list`/`locks`).

`Ctrl+C` in `watch` prints `(interrupted — session still running)`
without a summary — deliberate, so "I stopped watching" never reads as
"session ended".

**Liveness watchdog — check every ~10 minutes.** A 60m ceiling is a
long time to be blind, so don't just wait for the completion
notification. Probe the session is still alive periodically:

```
crush sessions locks <id>   # heartbeat: alive / ping / stopping / offline
```

This is a liveness probe, not output polling — the completion
notification still delivers the result. But if the heartbeat reads
`offline`/`stopping` with no completion notification, the holder died
silently: stop waiting, inspect `.crush/stdin/<task>.{out,err}` +
`crush sessions last <id>` (or `crush sessions why <id>`), and
re-launch into the same `--session` rather than burning the rest of
the 60m on a dead process.

**Tear the watchdog down when it has nothing left to watch.** The
10-minute cycle exists only to babysit live runs. Once a session
finishes — and you're not launching a replacement and no other
`/crush` runs are in flight — drop the liveness loop; an idle watchdog
is just noise. Re-arm it only when you launch the next run.

## Steering a running session — `sessions inject`

Hand a **new message to a run already in flight** in another process,
without killing and relaunching it — to correct course, add a
forgotten constraint, or answer a question the agent surfaced mid-run.

```
crush sessions inject <id> -m "also update the CHANGELOG"     # merge
crush sessions inject <id> -f ./notes/next-step.md            # from a file
crush sessions inject <id> -m "stop — wrong approach" --interrupt
```

- `<id>` is the same `--session` id you launched with (short hash ok).
- **Default (merge):** spliced into the run's **next provider step** —
  current turn is NOT cancelled. Cheapest way to feed extra context;
  latency is one step.
- **`--interrupt`:** cancels the in-flight turn and immediately
  restarts it with the new message on top of everything produced so
  far. Use when the current direction is wrong and shouldn't finish.
- Persisted as a normal **user** message (shows up in
  `sessions watch`/`last` and the web UI exactly as if typed there).
- If the session is **not currently running**, the message is still
  persisted and picked up next time that session id runs — the
  command tells you so instead of failing.

Works cross-process: writes to the session DB, and the running `crush
run` (or web server) owning that session picks it up. Add `--json` for
a machine-readable `{session_id, message_id, running, status}` result.

## Repo-wide default system prompt

If `./.crush/system-prompts/default.md` exists, `crush run` auto-loads
it as the system prompt when neither `--system-prompt` nor
`--system-prompt-file` was passed. Use it to commit one set of "always
apply" rules per repo (stay in scope, end with a final assistant
message, never commit/push, run the tests, surface ambiguity). Explicit
`--system-prompt-file` always wins.

## When the lock is stuck

Don't `rm` the lock manually — on Windows the OS still holds the
handle and refuses. Use:

```
crush sessions kill <id>            # kills holder PID + removes lock
crush sessions kill <id> --wait 10s # extra time for a slow holder
crush sessions reset <id> --force   # same + wipe message history
crush sessions reap                 # sweep ALL orphan locks at once
```

On Windows `kill` goes through `taskkill /F /T /PID` (whole tree:
`crush.exe` → `claude.cmd` → `node.exe`) and polls until the PID
exits, then retries lock removal until the OS releases the handle.

## After the run finishes — you are responsible for verifying everything

The sub-agent's `final_text` and the JSON envelope are CLAIMS, not
receipts. **Zero trust. You are obliged to verify the result yourself**
before reporting back — the envelope is not evidence of what actually
happened.

Check, with your own eyes:

- **The actual diff** vs the asked task — every changed file, every
  hunk, scope and intent. Out-of-scope edits and claim-vs-diff
  mismatches must be dealt with.
- **Any tests added or modified** — do they really exercise the
  bug/feature, or are they vacuous (assert-nothing, tautological
  mocks, pass-against-the-bug)? A test that doesn't fail without the
  fix has zero regression value.
- **The tests, re-run by you** — don't accept "tests pass" from the
  envelope. If flaky, prove it before dismissing.
- **Unfinished work papered over** — TODO/FIXME/placeholders in the
  diff, half-wired features that compile but don't connect
  end-to-end, mocked-out branches.
- **Build/lint/typecheck still clean** — one file's change can break a
  caller the sub-agent didn't touch.

If anything is off, **re-delegate** into the same `--session` with a
tighter prompt naming exactly what was wrong. Don't paper over the gap
yourself unless it's a true one-liner — fixing model output by hand
teaches the loop nothing.

Only report back **after** you have personally seen the diff and the
test run. **Never echo the sub-agent's claim verbatim** — your
authority is your verification, not the envelope. Report what was
actually done, what *you* ran, and any compromises or re-delegations.

### Where to find envelope details

- `.crush/stdin/<task>.out` — wire envelope (`--json`) or final text.
  Read first.
- `.warnings[]` — `final_text is empty` means the model ended on a
  `tool_call`; fall back to `git status` + `crush sessions last <id>`.
- `crush sessions watch <id>` — confirm the process really exited.
  Lock-alive heartbeat is the truth.

## Task

$ARGUMENTS
