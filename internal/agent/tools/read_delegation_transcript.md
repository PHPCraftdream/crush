Read the full, detailed message history of a PAST sub-agent delegation.

When you delegate work through the `agent` tool, you only get back that
sub-agent's FINAL result — its own step-by-step transcript (the tools it ran,
the files it read, its intermediate reasoning) is NOT injected into your
context. Use this tool when a delegation's result is surprising, incomplete, or
wrong and you need to understand WHY: it returns the child session's own
message history so you can inspect exactly what the sub-agent did.

Input:
- `session_id`: the child session id of the delegation to inspect. This is the
  exact "session ..." id shown in a prior `agent` tool result (for example the
  "SUB-AGENT QUESTION (session <id>)" note). It must be a delegation of YOUR
  session — reading an unrelated or top-level session is refused.

Output: a compact transcript of the sub-agent session — one block per message
(role, text, thinking previews, tool calls, and tool results), with long tool
outputs summarised to keep the response bounded.

Notes:
- This is a read-only inspection tool. It never modifies the sub-agent session.
- Only the orchestrator has this tool; a sub-agent does not, because inspecting
  its own delegation history is not meaningful.
