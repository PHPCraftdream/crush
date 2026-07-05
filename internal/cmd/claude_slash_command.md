---
description: Delegate this task to a crush sub-agent instead of doing it yourself
---

This skill is **opt-in only** — the user invokes it by typing
`/crush <task>` (or saying "delegate this to crush"). Inside that
single invocation you build a `crush run` and launch it. **Do NOT
auto-invoke on later turns** of the same chat: the user already chose
to do those follow-ups with you directly when they didn't type
`/crush` again.

## When NOT to delegate (even after /crush)

Refuse, say so in one sentence, and offer to do it directly:

- **Interactive by nature** — merges, conflict resolution, design or
  debugging that needs back-and-forth. A sub-agent cannot stop and ask.
- **Depends on this chat's context** — files you just read, decisions
  made earlier, partial state. A sub-agent starts cold.
- **The user gave the answer** — "fix it like this" / "use approach X"
  wants *your* hands so you can verify the assumption.
- **One- or two-line work** — delegation overhead > the change.
- **Mid-plan with the user** — keep the plan coherent in one head.

## Fallback when `crush` hits rate limits

Fall back to a local `Agent` sub-agent **only** when `crush` has hit a
hard window / quota limit that won't recover by retrying — the user's
weekly / monthly token budget is gone, the account is suspended, or the
provider says "context window exceeded" / "quota exceeded" with no
retry-after that would land inside this session.

Re-route immediately in those cases, without asking for confirmation:

- Complex / non-trivial work → `@ao46l` (agent) — Opus, heavier model.
- Simple / one-liner / mechanical task → `@ash` (agent) — Sonnet, faster.

Brief the sub-agent the same way you would have briefed `crush`: state
the goal, the file-set it may touch, and what "done" looks like. The
zero-trust verification rule below still applies — verify the diff and
re-run the tests yourself.

**Do NOT fall back** for transient or recoverable failures:

- a `--timeout` you yourself set fired — re-run `crush run` against the
  **same `--session` id** with a larger `--timeout`.
- a situational HTTP 429 with short retry-after (per-minute / per-second
  throttle, concurrent-request cap) — wait the retry-after and re-run.
- 5xx / network blip — re-run; if it persists, escalate to the user.
- operator-side errors (bad flag, missing workspace, malformed prompt)
  — fix the invocation and retry `crush`.

The local-agent fallback is the **last resort**, not a shortcut around
transient failures.

## Launching

- `--role smart` for non-trivial, `--role fast` for one-liners.
- Stable, task-meaningful `--session` id (issue / branch / topic slug)
  — same id continues across runs and is recognisable in `sessions watch`.
- `--timeout 60m` as the standard ceiling — set it on every run. It's
  generous on purpose: a mid-edit timeout leaves partial state, so a
  long ceiling is cheap insurance (the run still ends as soon as the
  task is done, well before 60m). Only drop lower for a genuinely tiny
  task where you want a fast failure signal.
- Run in the background (`Bash` `run_in_background: true`), redirect to
  `.crush/stdin/<task>.{out,err}`, react on the completion notification.
  Don't sleep-poll for output — but do run the liveness watchdog below.
- Multi-line prompts → `Write` to `.crush/stdin/<task>.prompt`, feed via
  `< file`. Avoid positional `"…"` past one line.
- Permissions inside `crush run` are auto-approved — run only in
  workspaces you can afford to lose.
- **Parallel runs** MUST name the file-set each prompt may touch
  ("only edit `internal/foo/`"). Two runs writing the same file race
  and corrupt silently.
- **Parallel runs MUST forbid git writes.** When more than one `crush
  run` is in flight against the same worktree, every prompt MUST tell the
  agent NOT to run git write commands — no `commit`, `add`, `stash`,
  `reset`, `checkout`/`restore`, `rebase`, `merge`. Concurrent index/tree
  writes clobber each other (`index.lock` races, one run's `checkout`
  reverting another's edits). Read-only git (`status`, `diff`, `log`) is
  fine. The orchestrator stages and commits **sequentially, itself**,
  after the runs finish and it has verified each diff. (A single solo run
  may still be told not to commit per the usual scope rules; this clause
  is specifically about the multi-run race.) When edits genuinely overlap
  and can't be serialized, give each run its own `git worktree` instead.

## Monitoring

`crush sessions watch` is the primary monitor — auto-detects end and
prints a summary (duration, tokens, cost). Unlike `sessions tail
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

Read-only secondaries: `sessions list` (with STATUS column),
`sessions locks` (heartbeat: `alive` / `ping` / `stopping` / `offline`),
`sessions show <id> --with-messages`, `sessions last <id> [--n N]`.

`Ctrl+C` in `watch` prints `(interrupted — session still running)`
without a summary — deliberate, so "I stopped watching" never reads
as "session ended".

**Liveness watchdog — check every ~10 minutes.** A 60m ceiling is a
long time to be blind, so don't just wait for the completion
notification. Every ~10 minutes while the run is in flight, probe that
the session is still alive:

```
crush sessions locks <id>   # heartbeat: alive / ping / stopping / offline
```

This is a liveness probe, not output polling — the completion
notification still delivers the result. But if the heartbeat reads
`offline` / `stopping` and no completion notification has arrived, the
holder died silently: stop waiting, inspect
`.crush/stdin/<task>.{out,err}` + `crush sessions last <id>`, and
re-launch into the same `--session` rather than burning the rest of
the 60m on a dead process.

**Tear the watchdog down when it has nothing left to watch.** The
10-minute cycle exists only to babysit live runs. Once a session
finishes — and you are not launching a replacement and no other
`/crush` runs are still in flight — drop the liveness loop entirely.
Don't keep probing `sessions locks` on an empty field; an idle
watchdog is just noise. Re-arm it only when you launch the next run.

## Steering a running session — `sessions inject`

You can hand a **new message to a run that is already in flight** in
another process, without killing and relaunching it. Use this to
correct course, add a constraint you forgot, or answer a question the
agent surfaced mid-run.

```
crush sessions inject <id> -m "also update the CHANGELOG"     # merge
crush sessions inject <id> -f ./notes/next-step.md            # from a file
crush sessions inject <id> -m "stop — wrong approach" --interrupt
```

- `<id>` is the same `--session` id you launched with (short hash ok).
- **Default (merge):** the message is spliced into the run's **next
  provider step** — the current turn is NOT cancelled. Cheapest way to
  feed extra context; latency is one step.
- **`--interrupt`:** cancels the in-flight turn and immediately restarts
  it with the new message on top of everything produced so far. Use
  when the current direction is wrong and you don't want it to finish.
- The message is persisted as a normal **user** message (it shows up in
  `sessions watch`/`last` and the web UI exactly as if you typed it).
- If the session is **not currently running**, the message is still
  persisted and picked up the next time that session id runs — the
  command tells you so instead of failing.

This works cross-process: it writes to the session DB and the running
`crush run` (or web server) owning that session picks it up. Add
`--json` for a machine-readable `{session_id, message_id, running,
status}` result.

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
before reporting back. The envelope is not evidence of what actually
happened.

Check, with your own eyes:

- **The actual diff** vs the asked task — every changed file, every
  hunk, scope and intent. Out-of-scope edits and claim-vs-diff
  mismatches must be dealt with.
- **Any tests added or modified** — do they really exercise the
  bug / feature, or are they vacuous (assert-nothing, tautological
  mocks, pass-against-the-bug)? A test that doesn't fail without
  the fix has zero regression value.
- **The tests, re-run by you** — don't accept "tests pass" from
  the envelope. If flaky, prove it before dismissing.
- **Unfinished work papered over** — TODO / FIXME / placeholders in
  the diff, half-wired features that compile but don't connect
  end-to-end, mocked-out branches.
- **Build / lint / typecheck still clean** — one file's change can
  break a caller the sub-agent didn't touch.

If anything is off, **re-delegate** into the same `--session` with a
tighter prompt naming exactly what was wrong. Don't paper over the
gap yourself unless it's a true one-liner — fixing model output by
hand teaches the loop nothing.

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
