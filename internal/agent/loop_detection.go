package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"

	"charm.land/fantasy"
)

const (
	loopDetectionWindowSize = 10
	loopDetectionMaxRepeats = 5
)

// loopDetail carries specifics about a detected loop so the assistant
// message's Finish part can record a distinguishable, operator-visible
// explanation instead of looking identical to a model that voluntarily
// finished. The finish REASON stays FinishReasonEndTurn (a loop-detected
// stop legitimately IS a form of "done" — the turn ended cleanly, on
// purpose — and reclassifying it to a distinct enum value would break the
// reclassifyCrashedAsDone / sessions-why logic that keys on EndTurn); the
// distinction is carried in the Finish part's message/details text instead.
type loopDetail struct {
	// ToolName is the name of the tool whose call signature repeated. May
	// be empty if the tool name could not be extracted.
	ToolName string
	// Count is how many times the signature appeared in the window when the
	// threshold was crossed (the value that exceeded maxRepeats).
	Count int
	// Threshold is the maxRepeats value the detection used.
	Threshold int
}

// hasRepeatedToolCalls checks whether the agent is stuck in a loop by looking
// at recent steps. It examines the last windowSize steps and returns true if
// any tool-call signature appears more than maxRepeats times. When it returns
// true, the returned loopDetail is populated with the tool name and count so
// callers (agent.go's OnStepFinish) can record a specific, operator-visible
// explanation on the assistant message's Finish part.
func hasRepeatedToolCalls(steps []fantasy.StepResult, windowSize, maxRepeats int) (bool, loopDetail) {
	var detail loopDetail
	detail.Threshold = maxRepeats
	if len(steps) < windowSize {
		return false, detail
	}

	window := steps[len(steps)-windowSize:]
	counts := make(map[string]int)
	toolNames := make(map[string]string) // signature -> tool names for logging

	for _, step := range window {
		sig := getToolInteractionSignature(step.Content)
		if sig == "" {
			continue
		}
		counts[sig]++
		if _, exists := toolNames[sig]; !exists {
			// Extract tool names for logging
			calls := step.Content.ToolCalls()
			if len(calls) > 0 {
				toolNames[sig] = calls[0].ToolName
			}
		}
		if counts[sig] > maxRepeats {
			// Operator-visible: loop detection truncating a turn is a
			// meaningful event (a legitimate polling pattern may have been
			// cut short), not routine debug noise. Other notable agent
			// events in agent.go (max-cost/max-tokens aborts, empty-stream)
			// use Warn.
			slog.Warn("Loop detected — stopping to avoid infinite repeat",
				"tool", toolNames[sig], "count", counts[sig], "threshold", maxRepeats)
			detail.ToolName = toolNames[sig]
			detail.Count = counts[sig]
			return true, detail
		}
	}

	return false, detail
}

// loopDetectedFinishText builds the message + details strings for the Finish
// part recorded when loop detection stops a turn. Extracted as a named helper
// so it is directly unit-testable without driving a full agent.Run(). Both
// strings are non-empty whenever detail is non-zero — this is the property the
// regression test asserts, since it is what distinguishes a loop-detected stop
// from a voluntary model finish in the persisted message.
func loopDetectedFinishText(detail loopDetail) (msg, details string) {
	tool := detail.ToolName
	if tool == "" {
		tool = "(unknown)"
	}
	msg = fmt.Sprintf("Stopped: repeated identical `%s` tool calls (%d in a row)", tool, detail.Count)
	details = fmt.Sprintf(
		"The agent made the same `%s` tool call with the same result %d times "+
			"in a row (loop-detection threshold is > %d) and was stopped to avoid an "+
			"infinite loop. If this was legitimate polling of a background job or a "+
			"resource that legitimately returns byte-identical output, the task may "+
			"be unfinished — re-run it, possibly with a sleep or a varying argument "+
			"to avoid tripping the detector.",
		tool, detail.Count, detail.Threshold,
	)
	return msg, details
}

// getToolInteractionSignature computes a hash signature for the tool
// interactions in a single step's content. It pairs tool calls with their
// results (matched by ToolCallID) and returns a hex-encoded SHA-256 hash.
// If the step contains no tool calls, it returns "".
func getToolInteractionSignature(content fantasy.ResponseContent) string {
	toolCalls := content.ToolCalls()
	if len(toolCalls) == 0 {
		return ""
	}

	// Index tool results by their ToolCallID for fast lookup.
	resultsByID := make(map[string]fantasy.ToolResultContent)
	for _, tr := range content.ToolResults() {
		resultsByID[tr.ToolCallID] = tr
	}

	h := sha256.New()
	for _, tc := range toolCalls {
		output := ""
		if tr, ok := resultsByID[tc.ToolCallID]; ok {
			output = toolResultOutputString(tr.Result)
		}
		io.WriteString(h, tc.ToolName)
		io.WriteString(h, "\x00")
		io.WriteString(h, tc.Input)
		io.WriteString(h, "\x00")
		io.WriteString(h, output)
		io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// toolResultOutputString converts a ToolResultOutputContent to a stable string
// representation for signature comparison.
func toolResultOutputString(result fantasy.ToolResultOutputContent) string {
	if result == nil {
		return ""
	}
	if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result); ok {
		return text.Text
	}
	if errResult, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result); ok {
		if errResult.Error != nil {
			return errResult.Error.Error()
		}
		return ""
	}
	if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result); ok {
		return media.Data
	}
	return ""
}
