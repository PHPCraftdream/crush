package agent

import (
	"context"
	"log/slog"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
)

// Every tool failure reaches the log, whichever way it fails.
//
// Before this wrapper existed, none of them did. A run died 42 seconds into
// a 75k-character prompt because a todos item carried an unusable status,
// and the whole failure window in crush.log contained no ERROR record at
// all — the only carriers were the process's stderr and the JSON envelope,
// and both are overwritten when the same session id is run again. Diagnosing
// it meant reading the source.
//
// The wrapper is deliberately unconditional, unlike wrapToolsWithHooks,
// which returns the slice untouched when no hook runner is configured or
// when the caller is a sub-agent. Those are exactly the cases the incident
// above fell into, so hooking the logging onto that wrapper would have left
// the original blind spot in place.
//
// It sits OUTSIDE the hook wrapper, so a tool call a hook refuses is logged
// too: from an operator's point of view "the hook blocked it" and "the tool
// rejected it" are the same question — why did this call not do anything.
type loggedTool struct {
	inner fantasy.AgentTool
}

func newLoggedTool(inner fantasy.AgentTool) *loggedTool {
	return &loggedTool{inner: inner}
}

// wrapToolsWithErrorLogging wraps every tool so its failures are logged.
// Applied to all tools on every path — no runner, no sub-agent, no
// configuration gates it.
func wrapToolsWithErrorLogging(list []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(list))
	for i, tool := range list {
		out[i] = newLoggedTool(tool)
	}
	return out
}

func (l *loggedTool) Info() fantasy.ToolInfo { return l.inner.Info() }

func (l *loggedTool) ProviderOptions() fantasy.ProviderOptions {
	return l.inner.ProviderOptions()
}

func (l *loggedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	l.inner.SetProviderOptions(opts)
}

func (l *loggedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := l.inner.Run(ctx, call)

	// Levels are split on purpose. A returned error is level 3 in the tool
	// error contract (internal/agent/tools/tools.go): it unwinds the agent
	// loop and ends the run, so it is always ERROR. An error RESPONSE is
	// level 1 — the model is expected to misuse a tool occasionally and
	// correct itself, and logging every mistyped path at ERROR would drain
	// the level of meaning for the failures that actually end a session.
	// WARN keeps them greppable without that cost.
	//
	// The attribute is level_kind, not level: slog owns "level" in its own
	// output, and a second one would emit a duplicate JSON key.
	switch {
	case err != nil:
		slog.Error("tool call failed, ending the run",
			"tool", call.Name,
			"session_id", tools.GetSessionFromContext(ctx),
			"tool_call_id", call.ID,
			"level_kind", "fatal",
			"err", err,
		)
	case resp.IsError:
		slog.Warn("tool call returned an error to the model",
			"tool", call.Name,
			"session_id", tools.GetSessionFromContext(ctx),
			"tool_call_id", call.ID,
			"level_kind", "recoverable",
			"stop_turn", resp.StopTurn,
			"content", resp.Content,
		)
	}

	return resp, err
}
