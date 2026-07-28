package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"unicode/utf8"

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

const (
	// defaultTranscriptMaxMessages caps how many of the most-recent messages
	// are rendered by default. The orchestrator is usually interested in the
	// tail end of a delegation (the final result, recent decisions), not the
	// entire history.
	defaultTranscriptMaxMessages = 50

	// defaultTranscriptMaxBytes is the overall byte budget for the rendered
	// transcript output. If exceeded, output is truncated at a rune boundary
	// and a marker is appended indicating how many bytes were dropped —
	// similar to the boundedBuffer pattern in internal/shell/background.go.
	defaultTranscriptMaxBytes = 150_000
)

// ReadDelegationTranscriptParams is the JSON schema for the
// read_delegation_transcript tool.
type ReadDelegationTranscriptParams struct {
	// SessionID is the child session id of a past `agent` delegation — the
	// exact "session ..." id surfaced in a prior agent tool result (e.g. a
	// "SUB-AGENT QUESTION (session ...)" note) — whose transcript to read.
	SessionID string `json:"session_id" description:"The child session id of a past sub-agent delegation whose full message history to read. Must be a descendant of the current session — reading an unrelated session is refused."`
	// MaxMessages caps how many of the most-recent messages are included.
	// Defaults to 50. Use with Offset to page backward through earlier history.
	MaxMessages int `json:"max_messages,omitempty" description:"Maximum number of most-recent messages to include (default: 50). Older messages are omitted from the tail; increase Offset to page backward."`
	// MaxBytes is the overall byte budget for the rendered transcript.
	// Defaults to 150000. If exceeded, output is truncated with a marker.
	MaxBytes int `json:"max_bytes,omitempty" description:"Overall byte budget for the rendered transcript (default: 150000). If exceeded, output is truncated with a byte-count marker."`
	// Offset pages backward from the end of the message list. 0 (default)
	// shows the most recent messages; increasing it skips newer messages to
	// reveal earlier ones.
	Offset int `json:"offset,omitempty" description:"Number of messages to skip from the END of the list (default: 0 = most recent). Increase to page backward through earlier history."`
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
			isDescendant, err := isDescendantSession(ctx, sessions, callerID, target)
			if err != nil {
				// A DB error partway through the tree walk means we could NOT
				// verify ownership one way or the other — it does NOT mean
				// target isn't a descendant. This is a security-relevant check
				// (who may read whose transcript), so silently treating
				// "couldn't verify" the same as "verified not a descendant"
				// would produce a final-sounding refusal for what might be a
				// perfectly legitimate child session. Surface a real error so
				// the caller can retry instead.
				return fantasy.ToolResponse{}, fmt.Errorf("failed to verify session ownership for %q: %w", target, err)
			}
			if !isDescendant {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"session_id %q is not a sub-agent delegation of the current session; refusing to read a session that does not belong to this agent",
					target,
				)), nil
			}

			msgs, err := messages.List(ctx, target)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to list messages for session %q: %w", target, err)
			}

			return fantasy.NewTextResponse(renderDelegationTranscript(target, msgs, params)), nil
		},
	)
}

// isDescendantSession reports whether target is rootID itself's descendant —
// i.e. reachable by following parent_session_id links down from rootID. It
// walks the tree breadth-first with a visited-set and a visit cap so a
// degenerate or cyclic tree can never turn this into an unbounded DB sweep.
// Returns (false, nil) when rootID == target (a session is not its own
// descendant), which the caller treats separately.
//
// Returns a non-nil error when ListSubSessions fails on ANY node visited
// during the walk. This is deliberate: this is a security-relevant ownership
// check, so a transient DB error on one intermediate node must NOT be
// swallowed and treated as "that branch has no matching descendant" — doing
// so would make an entire subtree invisible and could misclassify a
// legitimate child session as "not a descendant" purely because we failed to
// look. The caller is expected to surface this as a real error rather than
// falling through to the final "not a sub-agent delegation" refusal, which
// would read as a deliberate, final denial instead of "we couldn't check".
func isDescendantSession(ctx context.Context, sessions session.Service, rootID, target string) (bool, error) {
	if rootID == "" || target == "" || rootID == target {
		return false, nil
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
			return false, fmt.Errorf("listing sub-sessions of %q: %w", id, err)
		}
		for _, child := range children {
			if child.ID == target {
				return true, nil
			}
			if _, seen := visited[child.ID]; !seen {
				queue = append(queue, child.ID)
			}
		}
	}
	return false, nil
}

// renderTranscriptMessage renders a single message into a compact transcript
// section. Extracted from renderDelegationTranscript so the byte-budget loop
// can render one message at a time and stop mid-stream when the budget is hit.
func renderTranscriptMessage(num, total int, msg message.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "── message %d/%d [%s] ──\n", num, total, msg.Role)
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
	return b.String()
}

// renderDelegationTranscript formats a child session's messages into a compact,
// human/agent-readable transcript with bounded output. Two limits are enforced:
//
//  1. max_messages: only the most recent N messages are rendered (configurable
//     via params.MaxMessages, default 50). An offset can page backward.
//
//  2. max_bytes: the total rendered output is capped (configurable via
//     params.MaxBytes, default 150000). If the budget is exceeded mid-message,
//     output is truncated at a rune boundary and a "[N bytes truncated]"
//     marker is appended — same pattern as internal/shell/background.go's
//     boundedBuffer.
func renderDelegationTranscript(sessionID string, msgs []message.Message, params ReadDelegationTranscriptParams) string {
	maxMessages := params.MaxMessages
	if maxMessages <= 0 {
		maxMessages = defaultTranscriptMaxMessages
	}
	maxBytes := params.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultTranscriptMaxBytes
	}
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}

	total := len(msgs)

	var b strings.Builder
	fmt.Fprintf(&b, "Sub-agent delegation transcript (session %s), %d message(s)", sessionID, total)
	if total == 0 {
		b.WriteString(":\n\n(no messages — the delegation has not produced any output yet)\n")
		return b.String()
	}

	// Compute the visible window. Offset trims from the END (newest); the
	// window is the maxMessages messages ending at (total - offset).
	end := total - offset
	if end < 0 {
		end = 0
	}
	start := end - maxMessages
	if start < 0 {
		start = 0
	}

	// Pagination marker when not showing everything.
	if start > 0 || offset > 0 {
		b.WriteString(" [")
		fmt.Fprintf(&b, "showing messages %d-%d", start+1, end)
		if start > 0 {
			fmt.Fprintf(&b, "; %d earlier omitted", start)
		}
		if end < total {
			fmt.Fprintf(&b, "; %d newer skipped (offset=%d)", total-end, offset)
		}
		b.WriteString("]")
	}
	b.WriteString(":\n\n")

	// Render window with byte budget. Reserve space for the truncation marker
	// so content + marker never materially exceeds maxBytes.
	const markerReserve = 256
	headerLen := b.Len()
	contentBudget := maxBytes - headerLen - markerReserve
	if contentBudget < 0 {
		contentBudget = 0
	}

	truncated := false
	var droppedBytes int
	omittedMsgs := 0
	for i := start; i < end; i++ {
		section := renderTranscriptMessage(i+1, total, msgs[i])
		written := b.Len() - headerLen
		remaining := contentBudget - written
		if remaining <= 0 {
			omittedMsgs = end - i
			truncated = true
			break
		}
		if len(section) <= remaining {
			b.WriteString(section)
			continue
		}
		// Truncate this section at the last valid UTF-8 rune boundary that
		// fits within the remaining budget.
		cut := remaining
		for cut > 0 && !utf8.RuneStart(section[cut]) {
			cut--
		}
		b.WriteString(section[:cut])
		droppedBytes += len(section) - cut
		omittedMsgs = end - i - 1
		truncated = true
		break
	}

	if truncated {
		b.WriteString("\n... [transcript truncated: ")
		if droppedBytes > 0 {
			fmt.Fprintf(&b, "%d bytes dropped in last message, ", droppedBytes)
		}
		fmt.Fprintf(&b, "%d subsequent message(s) omitted (max_bytes=%d)] ...\n", omittedMsgs, maxBytes)
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
