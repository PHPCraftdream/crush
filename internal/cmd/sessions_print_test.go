package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func erroredMessage() message.Message {
	msg := message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "tc1", Name: "agent", Input: "{}"},
		},
	}
	msg.AddFinish(message.FinishReasonError, "context canceled", "")
	return msg
}

// TestPrintMessage_ErrorFollowedByLaterIsRetried — an errored row that has a
// newer message after it in the session is labelled as retried, so the
// operator can tell a transient, auto-retried failure (coordinator
// transient-retry / orchestrator re-invoke / Phase-4 resume) from a terminal
// death. The raw enum still appears; only a suffix is added.
func TestPrintMessage_ErrorFollowedByLaterIsRetried(t *testing.T) {
	var buf bytes.Buffer
	printMessage(&buf, erroredMessage(), "text", nil, true)
	out := buf.String()
	assert.Contains(t, out, "(finished: error — retried, session continued)")
}

// TestPrintMessage_TerminalErrorStaysBare — the SAME finish_reason="error",
// but as the session's last row (nothing came after), must read as a bare
// terminal error with no retry note.
func TestPrintMessage_TerminalErrorStaysBare(t *testing.T) {
	var buf bytes.Buffer
	printMessage(&buf, erroredMessage(), "text", nil, false)
	out := buf.String()
	assert.Contains(t, out, "(finished: error)")
	assert.NotContains(t, out, "retried")
}

// TestPrintMessage_NonErrorFinishNotMangled — a non-error finish reason is
// never given the retry suffix, even when followed by a later message.
func TestPrintMessage_NonErrorFinishNotMangled(t *testing.T) {
	msg := message.Message{Role: message.Assistant}
	msg.AddFinish(message.FinishReasonEndTurn, "", "")
	var buf bytes.Buffer
	printMessage(&buf, msg, "text", nil, true)
	out := buf.String()
	assert.Contains(t, out, "(finished: end_turn)")
	assert.NotContains(t, out, "retried")
}

// TestPrintMessage_NDJSONRetriedField — the ndjson path exposes the retry
// signal as a separate boolean while keeping finish_reason as the raw enum.
func TestPrintMessage_NDJSONRetriedField(t *testing.T) {
	var buf bytes.Buffer
	printMessage(&buf, erroredMessage(), "ndjson", nil, true)
	out := buf.String()
	assert.Contains(t, out, `"finish_reason":"error"`)
	assert.Contains(t, out, `"retried":true`)

	var buf2 bytes.Buffer
	printMessage(&buf2, erroredMessage(), "ndjson", nil, false)
	out2 := buf2.String()
	assert.Contains(t, out2, `"finish_reason":"error"`)
	// omitempty → the field is absent (not "retried":false) on a terminal row.
	assert.NotContains(t, out2, "retried")
}

// TestFinishReasonLabel_Table exercises the pure label helper directly.
func TestFinishReasonLabel_Table(t *testing.T) {
	require.Equal(t, "error — retried, session continued",
		finishReasonLabel(message.FinishReasonError, true))
	require.Equal(t, "error",
		finishReasonLabel(message.FinishReasonError, false))
	require.Equal(t, "end_turn",
		finishReasonLabel(message.FinishReasonEndTurn, true))
	require.Equal(t, "canceled",
		finishReasonLabel(message.FinishReasonCanceled, true))
}

// TestPrintMessage_RendersReasoning checks that a message whose only
// content is a ReasoningContent block is shown as "[thinking] <line>"
// instead of a bare role header (which is what the bug report showed).
func TestPrintMessage_RendersReasoning(t *testing.T) {
	msg := message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "Working out the plan.\nNext step is X."},
		},
	}
	var buf bytes.Buffer
	printMessage(&buf, msg, "text", nil, false)
	out := buf.String()
	assert.Contains(t, out, "[assistant]")
	assert.Contains(t, out, "[thinking] Working out the plan.")
	assert.NotContains(t, out, "(no content yet)")
}

// TestPrintMessage_EmptyAssistantSaysNoContent covers the originally-
// reported case: an assistant row with no renderable parts (e.g. a
// streaming row that hasn't flushed text yet, or a partial Finish
// checkpoint). We want a friendly marker instead of a bare header.
func TestPrintMessage_EmptyAssistantSaysNoContent(t *testing.T) {
	msg := message.Message{
		Role:  message.Assistant,
		Parts: nil,
	}
	var buf bytes.Buffer
	printMessage(&buf, msg, "text", nil, false)
	out := buf.String()
	assert.Contains(t, out, "[assistant]")
	assert.Contains(t, out, "(no content yet)")
}

// TestPrintMessage_EmptyTextStillSaysNoContent — parts present but every
// renderable one is empty (TextContent with Text=""). Should also fall
// through to the marker.
func TestPrintMessage_EmptyTextStillSaysNoContent(t *testing.T) {
	msg := message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: ""},
			message.ReasoningContent{Thinking: ""},
		},
	}
	var buf bytes.Buffer
	printMessage(&buf, msg, "text", nil, false)
	assert.Contains(t, buf.String(), "(no content yet)")
}

// TestPrintMessage_TextSuppressesNoContentMarker — a non-empty text
// rendering must NOT trigger the "no content yet" marker.
func TestPrintMessage_TextSuppressesNoContentMarker(t *testing.T) {
	msg := message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	}
	var buf bytes.Buffer
	printMessage(&buf, msg, "text", nil, false)
	out := buf.String()
	assert.Contains(t, out, "hello")
	assert.NotContains(t, out, "(no content yet)")
}

// TestPrintMessage_ReasoningTruncated — a very long single line of
// reasoning gets truncated to the preview limit (200 runes + ellipsis).
func TestPrintMessage_ReasoningTruncated(t *testing.T) {
	long := strings.Repeat("x", 500)
	msg := message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: long},
		},
	}
	var buf bytes.Buffer
	printMessage(&buf, msg, "text", nil, false)
	out := buf.String()
	assert.Contains(t, out, "[thinking] ")
	assert.Contains(t, out, "…") // ellipsis from truncatePreview
	assert.NotContains(t, out, strings.Repeat("x", 500))
}
