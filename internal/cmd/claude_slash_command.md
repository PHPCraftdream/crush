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

## Launching

- `--role smart` for non-trivial, `--role fast` for one-liners.
- Stable, task-meaningful `--session` id (issue / branch / topic slug)
  — same id continues across runs and is recognisable in `sessions watch`.
- `--timeout` proportional to scope: small tweak → `5m`; new file
  → `10m`; multi-file refactor → `20m`; bug-hunt → `30m`. Over-provision
  when in doubt — a mid-edit timeout leaves partial state.
- Run in the background (`Bash` `run_in_background: true`), redirect to
  `.crush/stdin/<task>.{out,err}`, react on the completion notification.
  Don't poll with sleep.
- Multi-line prompts → `Write` to `.crush/stdin/<task>.prompt`, feed via
  `< file`. Avoid positional `"…"` past one line.
- Permissions inside `crush run` are auto-approved — run only in
  workspaces you can afford to lose.
- **Parallel runs** MUST name the file-set each prompt may touch
  ("only edit `internal/foo/`"). Two runs writing the same file race
  and corrupt silently.

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
