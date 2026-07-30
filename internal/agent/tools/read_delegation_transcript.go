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

	// hardMaxTranscriptMessages is the absolute ceiling on max_messages,
	// regardless of what the caller requests. 10x the default (50) — generous
	// enough for a deep inspection of a delegation, bounded enough that a
	// runaway request can never pull thousands of decoded rows into memory.
	hardMaxTranscriptMessages = 500

	// hardMaxTranscriptBytes is the absolute ceiling on max_bytes. 10x the
	// default (150000): worst-case rendered output stays near ~1.5 MiB even
	// if the model asks for a gigabyte, so a pathological request can never
	// balloon the orchestrator's context.
	hardMaxTranscriptBytes = 1_500_000

	// hardMaxTranscriptOffset is the absolute ceiling on offset. OFFSET makes
	// SQLite scan and discard the leading rows before returning the window, so
	// an absurd offset (10^9) is a cheap DoS against the DB thread. 100x the
	// message window ceiling (500): generous for paging deep into a very large
	// delegation while bounding the discarded-row scan to a fast sub-second
	// range. A real delegation exceeding this is effectively unbounded history
	// the orchestrator would never read in full anyway.
	hardMaxTranscriptOffset = 50_000
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

			// Page at the SQL layer instead of loading the entire history: count
			// the true total (for an accurate "N earlier omitted" marker) and read
			// only the requested window. Before this, messages.List decoded every
			// row of the session before the window was applied — a multi-MiB
			// transcript was fully materialised even when only the tail was shown.
			//
			// ListPaginatedSnapshot (not separate Count + ListPaginated calls) is
			// used deliberately: this tool's primary use case is reading the
			// transcript of a delegation that may still be RUNNING and writing new
			// messages concurrently. Two independent round trips (count, then
			// window) can each observe a different snapshot if a message is
			// inserted in between, making the "N earlier omitted" marker disagree
			// with the actual window, or making offset-based paging across two
			// separate tool calls skip/duplicate messages as the live delegation's
			// message list grows underneath the numeric offset. ListPaginatedSnapshot
			// pins one consistent snapshot for both the total and the window - see
			// its doc comment on message.Service for the full explanation.
			maxMessages, _, offset := clampTranscriptWindow(params)

			window, total, err := messages.ListPaginatedSnapshot(ctx, target, maxMessages, offset)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to list messages for session %q: %w", target, err)
			}

			// ListPaginated returns newest-first (DESC by created_at); reverse
			// into chronological order so the transcript reads oldest-to-newest,
			// matching the visible order callers saw before pagination moved to
			// the DB.
			for i, j := 0, len(window)-1; i < j; i, j = i+1, j-1 {
				window[i], window[j] = window[j], window[i]
			}

			return fantasy.NewTextResponse(renderDelegationTranscript(target, window, int(total), params)), nil
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

// markerReserve is the byte budget set aside for the "[transcript
// truncated: ...]" epilog so content + marker never materially exceeds
// maxBytes.
const markerReserve = 256

// clampTranscriptWindow normalises the caller-supplied window parameters:
// missing values fall back to the defaults, and absurdly large values are
// clamped to the hard ceilings so a model can never bypass the output-size
// guard by passing max_bytes=10^9 or max_messages=10^6.
func clampTranscriptWindow(params ReadDelegationTranscriptParams) (maxMessages, maxBytes, offset int) {
	maxMessages = params.MaxMessages
	if maxMessages <= 0 {
		maxMessages = defaultTranscriptMaxMessages
	}
	if maxMessages > hardMaxTranscriptMessages {
		maxMessages = hardMaxTranscriptMessages
	}
	maxBytes = params.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultTranscriptMaxBytes
	}
	if maxBytes > hardMaxTranscriptBytes {
		maxBytes = hardMaxTranscriptBytes
	}
	offset = params.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > hardMaxTranscriptOffset {
		offset = hardMaxTranscriptOffset
	}
	return maxMessages, maxBytes, offset
}

// transcriptBuffer wraps a strings.Builder with a content-byte budget. Each
// write checks the remaining budget BEFORE committing bytes, so a single
// enormous content part is truncated in place rather than being rendered
// whole into an intermediate buffer and trimmed afterwards — the old
// "assemble-then-cut" pattern doubled peak memory for one huge message
// (renderTranscriptMessage built the entire section, then the caller sliced
// it down to the budget).
type transcriptBuffer struct {
	b             *strings.Builder
	contentBudget int
	written       int
	droppedBytes  int
	truncated     bool
}

// writeStr writes s if it fits within the remaining content budget. It
// returns true when the whole string was written. Once the budget is
// exhausted it is a no-op (truncated stays latched). If s does not fit, a
// prefix truncated at the last valid UTF-8 rune boundary is written, the
// unwritten tail is accounted in droppedBytes, and truncated is latched —
// mirroring internal/shell/background.go's boundedBuffer rune handling.
func (tb *transcriptBuffer) writeStr(s string) bool {
	if tb.truncated {
		return false
	}
	remaining := tb.contentBudget - tb.written
	if remaining <= 0 {
		tb.truncated = true
		return false
	}
	if len(s) <= remaining {
		tb.b.WriteString(s)
		tb.written += len(s)
		return true
	}
	cut := remaining
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	tb.b.WriteString(s[:cut])
	tb.written += cut
	tb.droppedBytes += len(s) - cut
	tb.truncated = true
	return false
}

// renderMessageInto renders one message into tb, numbering it num of total.
// Header and every content part are written straight through the budgeted
// buffer (no per-message intermediate Builder). Returns wroteHeader: whether
// at least the header line was emitted, which the caller uses to decide
// whether a budget-induced stop omits this message entirely or only the
// messages after it.
func renderMessageInto(tb *transcriptBuffer, num, total int, msg message.Message) (wroteHeader bool) {
	wroteHeader = tb.writeStr(fmt.Sprintf("── message %d/%d [%s] ──\n", num, total, msg.Role))
	rendered := 0
	for _, part := range msg.Parts {
		switch p := part.(type) {
		case message.TextContent:
			if strings.TrimSpace(p.Text) == "" {
				continue
			}
			// Write the text directly; only append a newline when the text
			// was fully written, so a multi-MiB part never gets copied into a
			// fresh text+"\n" string just to add the separator.
			if tb.writeStr(p.Text) {
				if !strings.HasSuffix(p.Text, "\n") {
					tb.writeStr("\n")
				}
				rendered++
			}
		case message.ReasoningContent:
			if strings.TrimSpace(p.Thinking) == "" {
				continue
			}
			if tb.writeStr(fmt.Sprintf("[thinking] %s\n", truncatePreview(firstLine(p.Thinking), 200))) {
				rendered++
			}
		case message.ToolCall:
			input := strings.TrimSpace(p.Input)
			var ok bool
			if input != "" {
				ok = tb.writeStr(fmt.Sprintf("[tool: %s] %s\n", p.Name, truncatePreview(collapseWhitespace(input), 200)))
			} else {
				ok = tb.writeStr(fmt.Sprintf("[tool: %s]\n", p.Name))
			}
			if ok {
				rendered++
			}
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
			var ok bool
			if body != "" {
				ok = tb.writeStr(fmt.Sprintf("%s %s\n", prefix, body))
			} else {
				ok = tb.writeStr(fmt.Sprintf("%s\n", prefix))
			}
			if ok {
				rendered++
			}
		}
	}
	if rendered == 0 {
		tb.writeStr("(no content)\n")
	}
	if f := msg.FinishPart(); f != nil && f.Reason != "" {
		tb.writeStr(fmt.Sprintf("(finished: %s)\n", f.Reason))
	}
	tb.writeStr("\n")
	return wroteHeader
}

// renderDelegationTranscript formats a child session's messages into a compact,
// human/agent-readable transcript with bounded output. msgs MUST be the
// already-paginated window in chronological (ASC) order; total is the true
// message count for the session (used for the "N earlier omitted" marker and
// per-message X/total numbering). Two limits are enforced:
//
//  1. max_messages: applied by the caller at the SQL layer
//     (ListMessagesBySessionPaginated, DESC LIMIT/OFFSET); only the window is
//     passed in here.
//
//  2. max_bytes: the rendered output is capped (configurable via
//     params.MaxBytes, default 150000, hard ceiling 1.5 MiB). If the budget is
//     exceeded mid-message, output is truncated at a rune boundary and a
//     "[N bytes dropped ..., M subsequent message(s) omitted]" marker is
//     appended — same pattern as internal/shell/background.go's boundedBuffer.
func renderDelegationTranscript(sessionID string, msgs []message.Message, total int, params ReadDelegationTranscriptParams) string {
	_, maxBytes, offset := clampTranscriptWindow(params)
	windowSize := len(msgs)

	var b strings.Builder
	fmt.Fprintf(&b, "Sub-agent delegation transcript (session %s), %d message(s)", sessionID, total)
	if total == 0 {
		b.WriteString(":\n\n(no messages — the delegation has not produced any output yet)\n")
		return b.String()
	}

	// 1-based chronological position of the first message in the window, so
	// per-message numbering and the pagination marker stay accurate without
	// the caller having to pass the full history.
	firstNum := total - offset - windowSize + 1
	if firstNum < 1 {
		firstNum = 1
	}
	lastNum := firstNum + windowSize - 1

	if windowSize == 0 {
		fmt.Fprintf(&b, " [offset %d is past the end; no messages in window]", offset)
		b.WriteString(":\n\n(no messages in the requested window)\n")
		return b.String()
	}

	// Pagination marker when not showing everything.
	if firstNum > 1 || offset > 0 {
		b.WriteString(" [")
		fmt.Fprintf(&b, "showing messages %d-%d", firstNum, lastNum)
		if firstNum > 1 {
			fmt.Fprintf(&b, "; %d earlier omitted", firstNum-1)
		}
		if total > lastNum {
			fmt.Fprintf(&b, "; %d newer skipped (offset=%d)", total-lastNum, offset)
		}
		b.WriteString("]")
	}
	b.WriteString(":\n\n")

	// Render the window with a byte budget. Each part is written straight into
	// the budgeted buffer and checked against the remaining budget BEFORE
	// committing, so a single huge content part cannot blow past maxBytes.
	tb := &transcriptBuffer{b: &b, contentBudget: maxBytes - b.Len() - markerReserve}
	if tb.contentBudget < 0 {
		tb.contentBudget = 0
	}

	omittedMsgs := 0
	for idx, msg := range msgs {
		wroteHeader := renderMessageInto(tb, firstNum+idx, total, msg)
		if tb.truncated {
			if wroteHeader {
				// This message was partially shown; only later ones are omitted.
				omittedMsgs = windowSize - idx - 1
			} else {
				// Budget exhausted before this message's header: it is omitted too.
				omittedMsgs = windowSize - idx
			}
			break
		}
	}

	if tb.truncated {
		b.WriteString("\n... [transcript truncated: ")
		if tb.droppedBytes > 0 {
			fmt.Fprintf(&b, "%d bytes dropped in last message, ", tb.droppedBytes)
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
