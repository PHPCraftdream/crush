Ask the operator/orchestrator a question and stop the turn to wait for an answer.

<usage>
- Use when you are blocked on a decision only a human or the orchestrating
  agent can make (e.g. ambiguous requirements, a destructive/irreversible
  choice, missing credentials or access).
- Provide a specific, self-contained `question` — the turn ends immediately
  after this call, so whoever resumes the session sees only this text, not
  the rest of the conversation.
- `options` is optional: a short list of suggested answers (e.g. `["yes",
  "no", "dry-run only"]"). It is advisory only — any free-text answer is
  still accepted when the session resumes.
</usage>

<important>
Calling this tool ALWAYS ends the turn — it does not return control to you.
There is no synchronous way to block mid-turn for an answer in this fork:
non-interactive and web sessions both auto-approve permissions
unconditionally, so "ask a question" means "stop cleanly and hand back a
resume command", not "wait here". Do not call any other tool after this one
in the same turn — it will not run.
</important>

<tips>
- Only ask when you are genuinely blocked; prefer making a reasonable
  assumption and stating it if the answer is unlikely to change the outcome.
- Ask one question at a time — do not bundle multiple unrelated questions
  into a single call.
</tips>
