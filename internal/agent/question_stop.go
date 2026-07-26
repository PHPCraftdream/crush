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

// subAgentQuestionText builds the tool-result text runSubAgent returns when
// a sub-agent (the `agent` tool) stops on AwaitingAnswerError instead of
// finishing normally. Sibling to AwaitingAnswerGuidance above: that function
// is for the top-level session stopping crush run itself (nobody left to
// answer, process must exit); this one is for a sub-agent stopping while its
// orchestrator is still alive and mid-turn — the orchestrator can decide the
// answer itself and keep going without its own turn ending. Wording matters
// here: this is read by a model, and the whole point of the "return and
// resume" design (see docs/plans/2026-07-26-orchestrator-worker-e2e.md,
// section 3) is that a generic-looking error would make the parent think the
// sub-agent crashed and redo the work itself instead of answering — so this
// must read unambiguously as "the sub-agent is fine and paused, not failed".
func subAgentQuestionText(childSessionID string, ae *AwaitingAnswerError) string {
	optsLine := ""
	if len(ae.Options) > 0 {
		optsLine = "\nSuggested options: " + strings.Join(ae.Options, " | ")
	}
	return fmt.Sprintf(
		"SUB-AGENT QUESTION (session %s): %s%s\n"+
			"The sub-agent is paused with its context intact. To answer, call the "+
			"`agent` tool again with resume_session_id=\"%s\" and your answer as the "+
			"prompt. To abandon it instead, just continue on your own.",
		childSessionID, ae.Question, optsLine, childSessionID,
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
