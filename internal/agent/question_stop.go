package agent

import (
	"errors"
	"fmt"
	"strings"
)

// AwaitingAnswerGuidance returns the orchestrator-facing instruction
// paragraph for a question-tool stop — the turn is force-finished because
// the agent asked a question and this fork's `crush run` has no synchronous
// way to block mid-turn for an answer (both headless and web sessions
// auto-approve permissions unconditionally; there is no blocking
// permission-style channel to wait on). The turn must end cleanly instead,
// with enough information for an orchestrating agent (or a human operator)
// to answer and resume.
//
// This is the single source of truth for the guidance text, mirroring
// PeakHoursGuidance in peak_hours_stop.go: the question-tool's finish-part
// writer and any stderr/CLI surface should call this instead of composing
// their own prose, so the wording stays identical everywhere it appears.
func AwaitingAnswerGuidance(question string, options []string, sessionID string) string {
	optsLine := ""
	if len(options) > 0 {
		optsLine = "\n\nSuggested options: " + strings.Join(options, " | ")
	}
	return fmt.Sprintf(
		"QUESTION: %s%s\n\n"+
			"This is not a crash — crush is intentionally stopping this turn because "+
			"the agent asked a question and needs an answer before it can continue. "+
			"crush is exiting now; it will not retry or re-ask on its own.\n\n"+
			"If an orchestrating agent is driving this session: decide the answer "+
			"yourself (or ask the human operator if it's genuinely their call), then "+
			"resume with:\n\n"+
			"  crush run --session %s \"<your answer>\"\n\n"+
			"This is a normal continuation, not a retry — do not treat exit_reason "+
			"\"awaiting_answer\" as a failure.",
		question, optsLine, sessionID,
	)
}

// awaitingAnswerStoppedFinishText builds the (msg, details) pair recorded as
// the Finish part when a session is force-stopped because the agent called
// the question tool. Mirrors peakHoursStoppedFinishText in
// peak_hours_stop.go: msg is a short, unambiguous headline (never empty —
// an empty message looks identical to a voluntary finish), details carries
// the underlying error verbatim plus the full orchestrator guidance.
func awaitingAnswerStoppedFinishText(err error) (msg, details string) {
	msg = "Stopped: agent asked a question and is awaiting an answer"
	var ae *AwaitingAnswerError
	question := ""
	var options []string
	sessionID := ""
	if errors.As(err, &ae) {
		question = ae.Question
		options = ae.Options
		sessionID = ae.SessionID
	}
	details = fmt.Sprintf("%s\n\n%s", err.Error(), AwaitingAnswerGuidance(question, options, sessionID))
	return msg, details
}
