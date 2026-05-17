package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fork-only tests (orchestrator UX): cover the pure-function piece of
// the sub-agent aggregation logic (lastAssistantText). The
// App-level helpers (collectSubAgentOutputs / subAgentSummaryStats)
// thin-wrap DB queries via the Sessions and Messages services; their
// integration is exercised end-to-end by `crush run` smoke runs and
// the buildRunResult tests in run_result_test.go, so we keep this
// file focused on the algorithmic core that doesn't need a temp DB.

// --- lastAssistantText --------------------------------------------------

func TestLastAssistantText_EmptySlice(t *testing.T) {
	assert.Equal(t, "", lastAssistantText(nil))
	assert.Equal(t, "", lastAssistantText([]message.Message{}))
}

func TestLastAssistantText_OnlyUserMessages(t *testing.T) {
	msgs := []message.Message{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "follow up"}}},
	}
	assert.Equal(t, "", lastAssistantText(msgs),
		"no assistant messages => empty text (sub-session never produced a reply)")
}

func TestLastAssistantText_AssistantUnfinishedSkipped(t *testing.T) {
	// An assistant message without a Finish part is mid-stream — not
	// usable as final output. Should be skipped in favour of an
	// earlier finished one (or empty if none).
	msgs := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "earlier complete"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "still streaming, no finish part"},
			},
		},
	}
	assert.Equal(t, "earlier complete", lastAssistantText(msgs),
		"walker must skip unfinished assistant message and return the earlier finished one")
}

func TestLastAssistantText_LastFinishedAssistantWins(t *testing.T) {
	msgs := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "first reply"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "follow up"}}},
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "second reply"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}
	assert.Equal(t, "second reply", lastAssistantText(msgs),
		"latest finished assistant message wins (newest-first walk)")
}

func TestLastAssistantText_MultiplePartsJoinedCorrectly(t *testing.T) {
	msgs := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Part one."},
				message.TextContent{Text: "Part two."},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		},
	}
	got := lastAssistantText(msgs)
	// FullText joins with "\n\n" — important for downstream readers
	// that grep section boundaries.
	assert.True(t, strings.Contains(got, "Part one."))
	assert.True(t, strings.Contains(got, "Part two."))
}

// --- maxSubAgentTextChars sanity ---------------------------------------

func TestMaxSubAgentTextChars_BoundedAndDocumented(t *testing.T) {
	// Bound the constant so we cannot accidentally bloat it past a
	// reasonable per-sub-agent ceiling. The current 64 KB × ~10
	// sub-agents = ~640 KB envelope worst case, which is fine; if a
	// future change ever pushes it past 1 MB the test fails loudly.
	require.Greater(t, maxSubAgentTextChars, 8*1024,
		"too tight: would cripple long sub-agent reports")
	require.Less(t, maxSubAgentTextChars, 1024*1024,
		"too loose: would blow up envelope on a fan-out audit")
}
