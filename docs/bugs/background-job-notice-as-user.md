# Bug — background-job-completion notices render as user messages

**Status:** open (task #86). Found 2026-06-26.
**Observed in:** `D:\dev\rust\fs-sandbox\.crush`, session `73219465-e1f2-40f7-8ade-11f58b7737c7`.

## Symptom

The web UI shows what looks like **two messages from the user**, but the
operator only typed the first one. The second "user" bubble is actually:

```
[user]
Background job 009 (`cd winrsbox && cargo test --workspace --no-run 2>&1 | tail -30`) finished: exit 0, ran 3m13s.
...
```

## Root cause

This is **Phase 3** (`coordinator.notifyBackgroundJobDone`, commit `0d4da8c0`)
working as designed at the data layer but wrong at the presentation layer:

1. When a backgrounded command finishes, its summary
   (`backgroundJobSummary`) is delivered into the owning session via
   `InjectMessage` → `createUserMessage` with **`Role = message.User`**. This is
   intentional — the model must see it as fresh input and react to it.
2. The web renders **any** `Role == User` message as a human bubble. The
   injected job-completion notice is therefore visually indistinguishable from a
   message the operator typed → "two user messages".

The presence of this notice also proves the deployed binary already includes
Phase 3 (newer than `c74b60d3`), so the earlier "deploy gap" note is partly
stale.

## Why the Phase 4 badge does not cover it

The `↻ auto-resumed` badge (Phase 4, commit `8dc66b47`, `message.AutoResumed`)
is set **only** on the auto-resume path: the goroutine in
`notifyBackgroundJobDone` tags its context with `autoResumedCtxKey{}` before
`c.Run`, and `createUserMessage` reads it. The **Phase 3 `InjectMessage` path**
(used when autonomy is OFF — the default — or the session is busy) never sets
that key, so the notice is persisted with `AutoResumed = false` and gets no
badge.

## Proposed fix

Mirror the `AutoResumed` end-to-end flag pattern (the exact template is commit
`8dc66b47`):

1. **DB:** migration `..._add_background_job_notice_to_messages.sql`
   (`ALTER TABLE messages ADD COLUMN background_job_notice INTEGER DEFAULT 0 NOT NULL`),
   `internal/db/sql/messages.sql`, then sqlc-regen (or hand-edit, mirroring
   `hidden`) `models.go` + `messages.sql.go` (all 5 Scans!).
2. **message model:** `CreateMessageParams.BackgroundJobNotice` + `Message`
   field + `Create` int64 coercion + `fromDBItem` scan.
3. **wire:** `MessageWire.BackgroundJobNotice` + assignment.
4. **coordinator:** set the flag when building/injecting the job-completion
   message for **both** paths — the Phase 3 `InjectMessage` delivery and the
   Phase 4 auto-resume `c.Run`. (Thread via `CreateMessageParams` for the inject
   path, and via a second context key — or fold both into one — for the Run
   path.)
5. **web:** `types.ts` `BackgroundJobNotice: boolean` + a muted badge in
   `Message.tsx`, e.g. **"⚙ background job finished"**, so the operator sees the
   notice is a system injection, not their own message. (Phase 4 auto-resume can
   keep the additional `↻ auto-resumed` badge to show it *also* started a turn.)

## Test-schema gotcha (do not repeat)

Three places hand-roll the `messages` schema for tests:
`internal/message/message_test.go`, `internal/cmd/sessions_show_test.go`,
`internal/session/session_test.go`. Adding a `NOT NULL` column to the real
migration breaks any of these test schemas that drive `CreateMessage` —
`internal/cmd` was missed once for `auto_resumed` and only the full
`go test ./...` (not the targeted package run) caught it (commit `ef52ce8e`).
Update all relevant test schemas and verify with the full pre-push including
`go test -race`.
