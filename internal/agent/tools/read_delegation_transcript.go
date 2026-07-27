package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// ReadDelegationTranscriptToolName is the tool name the orchestrator calls to
// fetch a PAST sub-agent delegation's own detailed message history.
const ReadDelegationTranscriptToolName = "read_delegation_transcript"

//go:embed read_delegation_transcript.md
var readDelegationTranscriptDescription string

// readDelegationTranscriptMaxSubtreeWalk caps how many sessions the ownership
// walk visits, defending against a pathological fan-out or an accidental
// parent/child cycle in the data — same ceiling and rationale as the
// call-tree walk in internal/cmd/sessions_activity.go.
const readDelegationTranscriptMaxSubtreeWalk = 512

// ReadDelegationTranscriptParams is the JSON schema for the
// read_delegation_transcript tool.
type ReadDelegationTranscriptParams struct {
	// SessionID is the child session id of a past `agent` delegation — the
	// exact "session ..." id surfaced in a prior agent tool result (e.g. a
	// "SUB-AGENT QUESTION (session ...)" note) — whose transcript to read.
	SessionID string `json:"session_id" description:"The child session id of a past sub-agent delegation whose full message history to read. Must be a descendant of the current session — reading an unrelated session is refused."`
}

// NewReadDelegationTranscriptTool builds the read_delegation_transcript agent
// tool. It lets the ORCHESTRATOR inspect a worker sub-agent's own detailed
// transcript on demand (for debugging why a delegation did something
// unexpected), WITHOUT that transcript ever being auto-injected into the
// orchestrator's context: the orchestrator normally only sees a delegation's
// final result via the `agent` tool's return value. This tool is the explicit,
// opt-in way to look deeper.
//
// SECURITY: the requested session id is verified to be a descendant of the
// CALLING session before any of its messages are returned — mirroring the
// ownership check in coordinator.runSubAgent's resume path — so a model
// cannot pass an arbitrary session id (someone else's session, or an
// unrelated top-level session) and read its contents. A bad id is a model
// mistake, not a crash: it returns a normal tool error (nil Go error) so the
// caller can correct itself rather than aborting the turn.
//
// The tool is deliberately gated to the orchestrator only (it is NOT in the
// read-only sub-agent toolset nor the worker toolset — see
// resolveReadOnlyTools / workerToolNames): a sub-agent inspecting its OWN
// delegation history is meaningless, while an orchestrator inspecting a
// worker's history is the whole point.
func NewReadDelegationTranscriptTool(sessions session.Service, messages message.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ReadDelegationTranscriptToolName,
		readDelegationTranscriptDescription,
		func(ctx context.Context, params ReadDelegationTranscriptParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			target := strings.TrimSpace(params.SessionID)
			if target == "" {
				return fantasy.NewTextErrorResponse("session_id is required"), nil
			}

			callerID := GetSessionFromContext(ctx)
			if callerID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session id missing from context")
			}

			if target == callerID {
				return fantasy.NewTextErrorResponse(
					"session_id refers to the current session, not a sub-agent delegation; pass the child session id from an `agent` tool result",
				), nil
			}

			// Ownership check: the target must be a descendant of the calling
			// session. Walk the caller's descendant tree (parent_session_id
			// chain) and confirm target is reached. This refuses arbitrary or
			// unrelated session ids the same way runSubAgent's resume path does.
			if !isDescendantSession(ctx, sessions, callerID, target) {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"session_id %q is not a sub-agent delegation of the current session; refusing to read a session that does not belong to this agent",
					target,
				)), nil
			}

			msgs, err := messages.List(ctx, target)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to list messages for session %q: %w", target, err)
			}

			return fantasy.NewTextResponse(renderDelegationTranscript(target, msgs)), nil
		},
	)
}

// isDescendantSession reports whether target is rootID itself's descendant —
// i.e. reachable by following parent_session_id links down from rootID. It
// walks the tree breadth-first with a visited-set and a visit cap so a
// degenerate or cyclic tree can never turn this into an unbounded DB sweep.
// Returns false when rootID == target (a session is not its own descendant),
// which the caller treats separately.
func isDescendantSession(ctx context.Context, sessions session.Service, rootID, target string) bool {
	if rootID == "" || target == "" || rootID == target {
		return false
	}
	visited := make(map[string]struct{}, 8)
	queue := []string{rootID}
	visits := 0
	for len(queue) > 0 && visits < readDelegationTranscriptMaxSubtreeWalk {
		id := queue[0]
		queue = queue[1:]
		if _, seen := visited[id]; seen {
			continue
		}
		visited[id] = struct{}{}
		visits++

		children, err := sessions.ListSubSessions(ctx, id)
		if err != nil {
			continue
		}
		for _, child := range children {
			if child.ID == target {
				return true
			}
			if _, seen := visited[child.ID]; !seen {
				queue = append(queue, child.ID)
			}
		}
	}
	return false
}

// renderDelegationTranscript formats a child session's messages into a compact,
// human/agent-readable transcript. It reuses the same one-line-per-part shape
// the CLI session renderers use (role header, text, [thinking], [tool: …],
// [tool-result: …]) so the orchestrator sees a familiar layout. Long text is
// left intact — the orchestrator asked for the DETAILED history — but tool
// inputs/results are previewed rather than dumped in full to keep the response
// bounded.
func renderDelegationTranscript(sessionID string, msgs []message.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sub-agent delegation transcript (session %s), %d message(s):\n\n", sessionID, len(msgs))
	if len(msgs) == 0 {
		b.WriteString("(no messages — the delegation has not produced any output yet)\n")
		return b.String()
	}

	for i, msg := range msgs {
		fmt.Fprintf(&b, "── message %d/%d [%s] ──\n", i+1, len(msgs), msg.Role)
		rendered := 0
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case message.TextContent:
				if strings.TrimSpace(p.Text) == "" {
					continue
				}
				b.WriteString(p.Text)
				if !strings.HasSuffix(p.Text, "\n") {
					b.WriteByte('\n')
				}
				rendered++
			case message.ReasoningContent:
				if strings.TrimSpace(p.Thinking) == "" {
					continue
				}
				fmt.Fprintf(&b, "[thinking] %s\n", truncatePreview(firstLine(p.Thinking), 200))
				rendered++
			case message.ToolCall:
				input := strings.TrimSpace(p.Input)
				if input != "" {
					fmt.Fprintf(&b, "[tool: %s] %s\n", p.Name, truncatePreview(collapseWhitespace(input), 200))
				} else {
					fmt.Fprintf(&b, "[tool: %s]\n", p.Name)
				}
				rendered++
			case message.ToolResult:
				name := p.Name
				if name == "" {
					name = p.ToolCallID
				}
				prefix := "[tool-result: " + name + "]"
				if p.IsError {
					prefix += " ERROR"
				}
				body := summariseTranscriptResult(p.Content)
				if body != "" {
					fmt.Fprintf(&b, "%s %s\n", prefix, body)
				} else {
					fmt.Fprintf(&b, "%s\n", prefix)
				}
				rendered++
			}
		}
		if rendered == 0 {
			b.WriteString("(no content)\n")
		}
		if f := msg.FinishPart(); f != nil && f.Reason != "" {
			fmt.Fprintf(&b, "(finished: %s)\n", f.Reason)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// summariseTranscriptResult collapses a tool result's content into a bounded
// preview: single line as-is (capped), multiline as "first line (+N lines)".
// Keeps the transcript readable without dumping a whole file a `view` returned.
func summariseTranscriptResult(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	first := strings.TrimSpace(lines[0])
	if len(lines) == 1 {
		return truncatePreview(first, 300)
	}
	return truncatePreview(first, 260) + fmt.Sprintf(" (+%d lines)", len(lines)-1)
}

// collapseWhitespace flattens runs of whitespace (including newlines) into
// single spaces so a multiline tool input renders on one preview line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// firstLine returns the first non-empty, trimmed line of s, or "" when s is
// blank. Used to collapse a multi-line thinking block into a one-line preview.
func firstLine(s string) string {
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// truncatePreview shortens s to max runes, appending an ellipsis when
// truncation actually happened. Counts runes (not bytes) so multibyte
// characters are never cut mid-encoding.
func truncatePreview(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return string(runes[:1])
	}
	return string(runes[:max-1]) + "…"
}
