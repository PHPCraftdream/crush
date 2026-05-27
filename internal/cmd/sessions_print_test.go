package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
)

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
	printMessage(&buf, msg, "text", nil)
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
	printMessage(&buf, msg, "text", nil)
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
	printMessage(&buf, msg, "text", nil)
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
	printMessage(&buf, msg, "text", nil)
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
	printMessage(&buf, msg, "text", nil)
	out := buf.String()
	assert.Contains(t, out, "[thinking] ")
	assert.Contains(t, out, "…") // ellipsis from truncatePreview
	assert.NotContains(t, out, strings.Repeat("x", 500))
}
