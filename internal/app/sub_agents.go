package app

import (
	"context"
	"log/slog"

	"github.com/charmbracelet/crush/internal/message"
)

// Fork-only file (orchestrator UX): the sub-agent fan-out aggregation
// helper used by --aggregation=attach and by the reduction-loss
// warning. Inert to upstream — see CHANGELOG.fork.md (Section 4.K).
//
// Background: when a `crush run` model uses the `agent` tool to fan
// out, each dispatch lands a sub-session whose parent_session_id
// points at the calling session. After Run() returns, the parent
// composes a final assistant message that often *summarises* the
// sub-agent outputs instead of concatenating them verbatim. The
// orchestrator on top sees just final_text and never gets the lost
// detail. session-#3 (2026-05-17) audit feedback measured a 7×
// reduction in extreme cases (parent 1.4 KB vs sub-agent combined
// ~10 KB).
//
// Two consumers of the helper below:
//
//   1. collectSubAgentOutputs builds the SubAgentOutputs envelope
//      field when --aggregation=attach is on.
//   2. subAgentSummaryStats computes (count, totalChars) for the
//      "final_text is X% of combined sub-agent output" warning that
//      ALWAYS fires (no opt-in) when reduction looks dramatic.

// maxSubAgentTextChars caps how much of each sub-agent's final text
// we surface into the envelope. Anything bigger gets a trailing
// "[…truncated, see session <id> for full text]" so the envelope
// stays bounded. 64 KB per sub-agent × ~10 sub-agents = ~640 KB
// envelope worst case; orchestrators can paginate by querying the
// sub-sessions directly if they want more.
const maxSubAgentTextChars = 64 * 1024

// collectSubAgentOutputs queries the session DB for every sub-session
// whose parent is parentSessionID, then fetches each sub-session's
// last assistant FullText via the messages service. Truncates long
// outputs to maxSubAgentTextChars. Empty slice (not nil) is returned
// when there are no sub-sessions — keeps the envelope JSON shape
// consistent for orchestrators that always expect the array key.
//
// Failures to list messages for a single sub-session are logged at
// WARN and the entry is skipped — partial output is better than
// dropping the whole field on one bad row.
func (app *App) collectSubAgentOutputs(ctx context.Context, parentSessionID string) []subAgentOutput {
	subs, err := app.Sessions.ListSubSessions(ctx, parentSessionID)
	if err != nil {
		slog.Warn("collectSubAgentOutputs: list sub-sessions failed",
			"parent", parentSessionID, "err", err)
		return []subAgentOutput{}
	}
	out := make([]subAgentOutput, 0, len(subs))
	for _, sub := range subs {
		msgs, mErr := app.Messages.List(ctx, sub.ID)
		if mErr != nil {
			slog.Warn("collectSubAgentOutputs: list messages failed",
				"sub_session", sub.ID, "err", mErr)
			continue
		}
		text := lastAssistantText(msgs)
		truncated := text
		if len(truncated) > maxSubAgentTextChars {
			truncated = truncated[:maxSubAgentTextChars] +
				"\n\n[…truncated, see session " + sub.ID + " for full text]"
		}
		out = append(out, subAgentOutput{
			SessionID: sub.ID,
			Title:     sub.Title,
			FinalText: truncated,
			CharCount: len(text),
		})
	}
	return out
}

// lastAssistantText walks msgs newest-first (the messages slice is
// chronological, so we iterate from the end) and returns the FullText
// of the first finished assistant message it finds. Empty string when
// the sub-session never produced one (e.g. crashed before finishing).
func lastAssistantText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != message.Assistant {
			continue
		}
		if !m.IsFinished() {
			continue
		}
		return m.FullText()
	}
	return ""
}

// subAgentSummaryStats is the lightweight version: same query, but
// returns only (count, totalChars) and skips the per-row text copy
// + truncation work. Used by the always-on reduction-loss warning,
// which only needs to compare a ratio.
func (app *App) subAgentSummaryStats(ctx context.Context, parentSessionID string) (count int, totalChars int) {
	subs, err := app.Sessions.ListSubSessions(ctx, parentSessionID)
	if err != nil {
		return 0, 0
	}
	for _, sub := range subs {
		msgs, mErr := app.Messages.List(ctx, sub.ID)
		if mErr != nil {
			continue
		}
		text := lastAssistantText(msgs)
		if text == "" {
			continue
		}
		count++
		totalChars += len(text)
	}
	return count, totalChars
}
