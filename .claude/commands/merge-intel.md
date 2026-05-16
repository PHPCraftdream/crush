---
description: Pull upstream/main into the fork with intent-driven conflict resolution.
---

# /merge-intel — intent-driven upstream merge

You are about to merge `origin/main` into the current fork branch. The goal is
**not** "make conflicts go away" — it is to understand the upstream author's
intent at each conflict, weigh it against what this fork actually needs, and
record the decision so the next merge is faster.

Read this whole file before doing anything. It is a playbook, not just a list.

## Pre-flight

1. **Read `CHANGELOG.fork.md`** in full — especially Section 2 (per-file
   conflict-resolution table). That table is the answer key. If it says
   "delete", you delete; if it says "keep ours", you keep ours. Only when
   the table is silent do you fall through to the procedure below.
2. **Working tree must be clean.** Run `git status` — abort if anything is
   uncommitted. Do not merge on top of in-progress work.
3. **Create a backup branch** named `backup/before-merge-$(date +%Y%m%d-%H%M%S)`.
   This is your escape hatch.
4. **Fetch.** `git fetch origin` and report how many commits we are behind:
   `git rev-list --count HEAD..origin/main`.
5. **Stop and confirm with the user** before starting the merge if the count
   is large (≥ 50) or if the previous merge happened more than a month ago.
   Tell them what's about to change at a high level (use
   `git log --oneline HEAD..origin/main | head -30`).

## Start the merge

```
git merge origin/main --no-edit
```

It will fail with conflicts. That is expected. Do **not** abort — work
through the conflicts.

List all conflict types:

```
git status --short | grep -E "^(UU|AA|DU|UD|AU|UA|DD)"
```

The two-letter prefixes mean:
- `UU` — both sides modified content
- `AA` — both sides added independently
- `DU` — we deleted, upstream modified
- `UD` — we modified, upstream deleted
- `AU` / `UA` — one side added, the other has unmerged state
- `DD` — both sides deleted

## Pass 1 — bulk-resolve from the answer key

Walk the list once and resolve every conflict that is covered by
**CHANGELOG.fork.md Section 2**. Typically:

- `internal/ui/*` (DU) → `git rm`. We don't ship the TUI.
- `internal/backend/`, `internal/client/`, `internal/cmd/server.go`,
  `internal/server/proto.go`, `internal/workspace/` (DU) → `git rm`.
- `go.mod` / `go.sum` (UU) → `git checkout --theirs go.mod go.sum && go mod tidy`.

After this pass, check what's left:

```
git status --short | grep -E "^(UU|AA|DU|UD|AU|UA|DD)"
```

If you also have **new** upstream files that landed in the working tree but
shouldn't (typically more TUI tests, logout helpers, race tests):

```
git diff --name-only --diff-filter=A HEAD | grep -E "(internal/ui/|logout|clientserver|cmd/server)"
```

— `git rm -f` them too. Same reasons as above.

## Pass 2 — content conflicts (UU / AA), one file at a time

For each remaining conflict, **do this exact sequence**. Do not shortcut it.

### A. Read the upstream commits

```
git log --oneline -5 origin/main -- <path>
```

This tells you which commits changed this file upstream and lets you read
their messages. If a commit message is just `chore: lint`, skip it. If it
says `fix(agent): drain queued messages after manual summarize`, that's a
behavioural change you must understand.

### B. Diff before / after

Look at both sides of the conflict:

```
git diff --merge-base HEAD origin/main -- <path>
```

— and the in-tree marker block (HEAD vs origin/main between `<<<<<<<`
markers).

### C. Write down three things — in your head or out loud

1. **What did the upstream author intend?** State it in one sentence
   without the words "they added X". Say *why* X.
   Example: "Upstream wants to detect a stale dev server by hashing the
   binary's mtime, replacing the old BuildTime stamp."
2. **What does our fork actually need?** From the WUI / WebSocket /
   embedded-server perspective. Not the same question.
   Example: "Our dev loop hits the same stale-server problem on hot
   rebuild, so the BuildID idea is also useful to us."
3. **Resolution.** Pick one of:
   - **Take theirs verbatim** — the intent matches ours.
   - **Take ours verbatim** — their intent does not apply to this fork.
   - **Adapt** — take their idea but rewrite to fit our architecture.
   - **Cherry-pick a slice** — keep most of ours, lift one specific block
     of theirs (e.g. the `--force` flag in `login.go`).

### D. Apply the resolution

Remove `<<<<<<<` / `=======` / `>>>>>>>` markers and leave the chosen code.

### E. Leave a `// Fork merge note:` only when the resolution is non-obvious

This is the rule the user enforced and it matters: **don't comment every
trivial pick**. If we kept our side of a TUI deletion, no comment needed —
that's covered by the file-level Fork patch header and CHANGELOG. If we
*adapted* upstream's idea, or *kept ours despite a useful-looking upstream
change*, leave a short comment:

```go
// Fork merge note (origin/main 9e126c27 "<commit subject>"): one
// sentence on why we picked this resolution. CHANGELOG section X.Y.
```

Refer to commit hashes, not branch state — hashes stay stable, branches
drift.

### F. Stage

```
git add <path>
```

Move on. Do not run the whole test suite between every file — it's slow.
Run `go build ./<package>/...` for a quick smoke check after the messy
ones (agent, message, permission).

## Pass 3 — verify

After Pass 2, no conflict markers should remain:

```
grep -RIn '^<<<<<<<\|^=======\|^>>>>>>>' --include='*.go' .
```

Then:

```
go build ./... 2>&1 | grep -v 'embed.go.*all:dist' | tail
go vet ./... 2>&1 | grep -v 'csync.*JSONSchemaAlias' | tail
```

If a build error is "method X not implemented" on a `mock*Service` struct,
the fix is to add the missing stub methods. Don't disable the test.

If a build error is "undefined: someVar", it's almost always upstream code
that leaked through an auto-merge into one of our functions. Read backup:

```
git show backup/before-merge-<timestamp>:<path>
```

— and either restore our pre-merge version of just that block, or delete
the upstream-only code.

Then key-package tests:

```
go test -count=1 -timeout=180s ./internal/agent/ ./internal/message/ \
    ./internal/permission/ ./internal/cmd/
```

If a test fails, **first run it against `backup/before-merge-<timestamp>`**:

```
git stash && go test -run <TestName> ./internal/<pkg>/ ; git stash pop
```

If it failed before the merge too, it's a pre-existing flake — note it in
the merge commit message but don't fix it during the merge. If it passed
before and fails now, that's a regression you introduced and you must fix
it before committing.

## Commit the merge — carefully

A botched commit at this stage is the single biggest risk. Specifically:

- Do **not** `git stash` after the merge started — it nukes `MERGE_HEAD`
  and the next commit becomes a regular commit, not a merge commit (one
  parent instead of two). This silently breaks the merge graph.
- Verify `.git/MERGE_HEAD` exists right before commit.
- If `MERGE_HEAD` is gone but the working tree is correct, recover with
  `git commit-tree`:

```
TREE=$(git write-tree)
COMMIT=$(git commit-tree $TREE \
    -p $(git rev-parse HEAD) \
    -p $(git rev-parse origin/main) \
    -m "Merge upstream/main into <branch> ...")
git update-ref HEAD $COMMIT
```

Confirm two parents:

```
git log -1 --pretty=format:"%P" HEAD   # must print two SHAs
```

The commit message should be a structured digest:

```
Merge upstream/main (N commits) into <branch>

N conflicts resolved per CHANGELOG.fork.md section 2. Key decisions:

- <file or group>: <one-line decision + reasoning>
- ...

Pre-existing flakes carried over: <test names>.
```

Do **not** push automatically. Tell the user it's ready and let them run
`git push fork <branch>`.

## After the merge

Append a new batch to **CHANGELOG.fork.md Section 6** (chronological log)
with the new fork commits this merge produced — including the merge commit
itself.

If you adopted any upstream idea that's worth documenting beyond the
inline `// Fork merge note:`, expand the matching section of
`CHANGELOG.fork.md` (sections 3 / 4 / 5).

## Anti-patterns

Things you must not do, no matter how tempting:

- `git checkout --theirs <file>` on a file we care about. It silently
  discards our changes — use it only on go.mod/go.sum where `go mod tidy`
  will reconcile anyway.
- "Just commit it, we'll fix tests later." No — tests that newly fail
  after the merge are the strongest signal that an intent was missed.
- Squashing the merge into a regular commit. The two-parent merge graph
  is how the *next* merger reads what was resolved and why.
- Force-pushing the merged branch to the public fork remote before the
  user has reviewed it. The merge graph is irrecoverable once it's out.

## Slash command epilogue

When you're done, tell the user — in one short sentence — what landed,
what was rejected, and what pre-existing tests still fail. Then stop. Do
not push.
