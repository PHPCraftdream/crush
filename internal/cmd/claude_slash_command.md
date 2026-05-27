---
description: Delegate this task to a crush sub-agent instead of doing it yourself
---

This skill is **opt-in only**. The user invokes it explicitly by typing
`/crush <task>` (or by saying "delegate this to crush", "use the crush
skill", etc.). Inside that single invocation you delegate the named task
to a `crush run` sub-agent instead of doing the work yourself.

**Do NOT auto-invoke this skill on later turns of the same conversation.**
Pattern-matching on "this looks like a task that could be delegated" is
wrong — the user already chose to do those tasks with you directly when
they did not type `/crush`. Only the current `/crush` invocation gets
delegated; everything before and after is your normal direct work.

## When NOT to delegate (even after /crush)

Refuse to delegate and explain why instead, when:

- **The task is interactive by nature** — upstream merges, conflict
  resolution, design discussions, debugging that needs back-and-forth.
  A sub-agent cannot stop and ask the user; it will charge ahead and
  make decisions you would have caught.
- **The task depends on this conversation's context** — files you just
  read, decisions made earlier in the chat, partial state you built up
  with the user. A sub-agent starts cold and has none of it. Either
  finish in the current chat or write a self-contained prompt that
  reconstructs the context — usually faster to just do it yourself.
- **The user gave the answer in their request** — when they said "fix
  it like this" or "use approach X", they want *your* hands on the
  keyboard so you can verify their assumption, not a sub-agent that
  will blindly apply it.
- **The task is one or two lines** — delegating costs ~10s of process
  startup + the wrapper overhead. Faster to just `Edit` it yourself.
- **You're inside a longer plan with the user** and this is one of its
  steps — keep the plan coherent in one head.

When refusing, say so in one sentence and offer to do it directly. Do
not start a sub-agent "just in case".

## Launching

Defaults to apply unless the user said otherwise:

- `--role smart` for non-trivial work; `--role fast` for one-liners.
- A stable, task-meaningful `--session` id (issue / branch / topic slug).
  Same id continues across runs — pick one you will recognise later in
  `crush sessions watch`.
- `--timeout` proportional to the scope. Rough rule of thumb:
  one-line tweak / single small file → `--timeout 5m`; new file
  under ~300 lines → `10m`; refactor across 2–4 files or any
  file over ~500 lines → `20m`; deep bug-hunt or multi-package
  → `30m`. When in doubt, over-provision — a 30m timeout costs
  nothing if the task finishes in 3m, but a 5m timeout that fires mid-edit
  leaves you with partial state.
- Launch in the background (`Bash` with `run_in_background: true`),
  redirect `> .crush/stdin/<task>.out 2>.crush/stdin/<task>.err`, and react
  when the harness fires the completion notification. Do NOT poll with
  sleep. (Yes, the folder is called `stdin/` even though it also holds
  `.out` and `.err` outputs — it's a single per-task working directory.
  Don't let the name confuse you.)
- For multi-line prompts, `Write` them to a file under
  `./.crush/stdin/<task-slug>.prompt` and feed via stdin (`< file`).
  Avoid positional `"…"` for anything past one line.
- Permissions inside `crush run` are auto-approved (no human at the
  keyboard). Run only in workspaces you can afford to lose.
- **Parallel runs**: when fan-out is more than one `crush run`, every
  prompt MUST explicitly name the file-set it is allowed to touch
  (e.g. "only edit `internal/foo/` and `docs/foo.md`; do not touch root
  configs"). Two concurrent runs writing the same file race each other's
  edits and produce silent corruption.

## Monitoring a running session

The primary command for watching a running session is `crush sessions
watch`. It auto-detects when the session ends and prints a summary
block (duration, tokens, cost) on exit — unlike `sessions tail
--follow`, which hangs forever on dead locks and gives no closure.

```
crush sessions watch              # interactive picker, then live-tail
crush sessions watch <id>         # live-tail one session directly
```

Live-tail shows every message as it arrives. Tool calls now render
their key argument inline so you can actually see what the agent is
doing:

```
[tool: bash] go test ./internal/agent/...
[tool-result: bash] no output
[tool: edit] internal/cmd/sessions.go
[tool-result: edit] <result> (+3 lines)
[tool: grep] "TODO" in internal/
[tool-result: grep] internal/cmd/run.go:142: // TODO: ...  (+8 lines)
```

End detection (any one terminates the watch):
- session row has a non-empty `EndedReason`,
- lock file disappeared AND ≥1 message exists,
- latest assistant message has a non-partial `Finish.Reason`.

`Ctrl+C` interrupts the watch without a summary — that's deliberate, so
"I stopped watching" never looks like "the session ended".

**Other useful read-only commands**:

```
crush sessions locks                       # heartbeat-based liveness table
crush sessions list                        # all sessions, with STATUS column
crush sessions show <id> --with-messages   # snapshot dump
crush sessions last <id>                   # last 10 messages
crush sessions last <id> --n 3             # last 3 messages
crush sessions tail <id>                   # last messages (one-shot, no follow)
```

`sessions locks` PULSE column (heartbeat every 10 s, stale after 20 s):
- `alive`    — last heartbeat ≤ 10 s ago, agent is running
- `ping`     — 10–15 s ago, likely still running
- `stopping` — 15–20 s ago, agent is finishing or slow
- `offline`  — >20 s ago, lock is stale (agent crashed or exited)

## Repo-wide default system prompt

If `./.crush/system-prompts/default.md` exists, `crush run`
auto-loads it as the system prompt when neither `--system-prompt`
nor `--system-prompt-file` was passed. Use this to commit one set
of "always apply" rules to the repo (scope-control, summary-required,
no-commits) instead of repeating them in every prompt.

Recommended starter template — write to `./.crush/system-prompts/default.md`:

```markdown
You are a sub-agent invoked by an orchestrator. Apply these rules to
every task in this repo, in addition to anything in the user prompt:

1. **Stay strictly in scope.** Edit ONLY the files the prompt names.
   Do not refactor unrelated code, generalise patterns, expand
   .gitignore beyond what's asked, or "tidy up" while you're nearby.
   If you notice unrelated mess, list it in your final summary and
   leave it untouched.
2. **End every turn with a final assistant message** that names:
   files you changed, tests you ran, and any noteworthy observation.
   Wrappers parse the final_text — leaving it empty silently is a bug.
3. **Never commit, never push** unless the prompt explicitly says so.
4. **Run the tests** that cover what you touched before declaring done.
5. If you hit an ambiguity that needs a real product decision, stop
   and surface it — don't guess and ship.
```

Explicit `--system-prompt-file` always wins over the auto-loaded
default.

## When the lock is stuck

If a session reports "session is already in use" but you know the holder
is dead or stuck (TaskStop killed only the shell wrapper, not the
underlying crush process; the box rebooted; previous run was
force-killed), do not try to `rm` the lock file manually — on Windows
the OS still considers it open and refuses with "the process cannot
access the file because it is being used". Use:

```
crush sessions kill <id>            # kills the holder PID + removes the lock
crush sessions kill <id> --wait 10s # give a slow holder more time to die
crush sessions reset <id> --force   # same, then also wipes message history
crush sessions reap                 # sweep ALL orphan locks (PID-dead) at once
```

On Windows the kill goes through `taskkill /F /T /PID` so the entire
child tree dies (typically `crush.exe` → `claude.cmd` → `node.exe`).
The command then polls until the PID actually exits and retries the
lock removal until the OS releases the file handle — no more "process
still using the file" loops, no need to fall back to `taskkill` by
hand.

After any of these, `crush run --session <id>` can re-enter cleanly.

## After the run finishes — review with zero trust

The sub-agent's `final_text` and the JSON envelope's claims are NOT
evidence of what actually happened. Models routinely:

- claim a fix that doesn't exist in the diff
- add a test that asserts trivial truths or tautologies and would
  pass against the bug (e.g. `assert.NoError(t, err)` without any
  actual exercise of the failing path)
- leave `TODO` / `XXX` / placeholder comments where they couldn't
  finish a piece
- edit files outside the asked scope ("tidying up" while nearby)
- mark a feature "done" with a half-implementation that compiles
  but doesn't wire end-to-end
- report tests as "green" without running them, or run only a
  subset and skip the slow integration ones

Run the steps below in order after EVERY `crush run`. Stop at the
first red flag and re-delegate (same `--session`) with a tighter
prompt — don't paper over the gap yourself unless it's a typo / one
import.

### Step 1 — `git diff` is the source of truth

```
git status --short                  # what files changed?
git diff <each-touched-file>        # read every hunk yourself
```

For each file IN scope of the task, confirm the change actually does
what was asked. For each file OUT of scope, decide: keep, or
`git checkout HEAD -- <file>` and re-prompt with tighter constraints.

Don't skim. Read the diff like a code review — if you cannot explain
in one sentence what each hunk does and why, re-delegate that part.

### Step 2 — read the new tests, not the test count

If the sub-agent added tests, OPEN them and read what they assert.
Watch for these failure modes:

- assertions that pass against the bug (asserting `err == nil` when
  the bug also returns `nil`; asserting a slice is non-empty without
  checking contents)
- `t.Skip` with stale or always-true conditions
- the test calls the function under test but never asserts its
  output — only that it didn't panic
- mocks that return exactly the value the test then checks for
  ("tautology tests")
- the test exercises a side effect (file written, DB row created)
  but never reads it back to verify
- the test is a syntactic copy of an existing one with a renamed
  function — same assertions, no new coverage

If a "new" test wouldn't fail against the pre-fix code, it has zero
regression value. Re-delegate with: "the tests you added do not
exercise the bug — write a test that fails against the previous
commit, then make it pass."

### Step 3 — run the tests yourself

```
go test -count=1 -timeout=180s ./<changed-package>/...
cargo test -p <changed-crate> --no-fail-fast
npm test --workspace=<changed-package>
pytest <changed-dir>
```

`-count=1` defeats Go's test cache so you actually run the new tests
rather than read a cached "PASS" from a previous run. Even if the
sub-agent reports the tests were green, run them ONCE MORE — the
sub-agent's run may have been optimistic, the cache may be lying,
or unrelated code may have shifted under the test since.

If a test fails, do NOT accept "must be a flake" without proof.
Re-run it three times. If still flaky, treat as broken and
re-delegate.

### Step 4 — sanity-check build / vet / lint / typecheck

```
go build ./...     &&  go vet ./...
cargo check        &&  cargo clippy -- -D warnings
tsc --noEmit       &&  eslint .
```

The sub-agent may have introduced an import or signature change that
compiles in the file it edited but breaks a downstream caller it
didn't touch.

### Step 5 — grep for surrender markers

```
git diff --no-color | grep -E "TODO|FIXME|XXX|unimplemented|placeholder|panic\\!"
```

These usually mean "I couldn't finish this part". If they appeared
in the sub-agent's diff (not pre-existing — compare with `git log
-1 -p` before the run), re-delegate the unfinished piece.

### Step 6 — only NOW report back to the user

Report:
- what the sub-agent ACTUALLY did, per your diff review (not per
  its envelope)
- what tests you ran yourself, with the exact command, and what
  passed / failed
- any compromises or out-of-scope edits you accepted, or that you
  reverted
- if you re-delegated mid-review, also report what the second pass
  changed

Never echo the sub-agent's claim verbatim — your authority comes
from the diff and the test run, not the envelope.

### Where to look up envelope details

- `.crush/stdin/<task>.out` — the wire envelope (with `--json`) or
  the model's final text (default mode). Always read this first.
- `.warnings[]` in the envelope — specifically
  `final_text is empty` means the model ended on a `tool_call`
  without composing a reply; fall back to `git status` and
  `crush sessions last <id>` for context.
- `crush sessions watch <id>` — to confirm the process really did
  exit (not still running). Lock-alive heartbeat is the truth.

(`crush sessions last <id>` and `crush sessions watch <id>` are only
needed when the `.out` file is missing, or for post-mortem audit of
an old session. For the just-finished run, the `.out` file already
has the envelope — read that.)

## Task

$ARGUMENTS
