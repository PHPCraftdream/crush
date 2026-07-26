package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// AskQuestionToolName is the tool name the model calls to stop the current
// turn and ask the operator/orchestrator a question.
const AskQuestionToolName = "ask_question"

//go:embed ask_question.md
var askQuestionDescription string

// AskQuestionParams is the JSON schema for the ask_question tool.
type AskQuestionParams struct {
	Question string   `json:"question" description:"The question to ask the operator/orchestrator. Be specific and self-contained: the turn ends immediately after this call, so whoever resumes the session only sees this text, not the preceding conversation."`
	Options  []string `json:"options,omitempty" description:"Optional suggested answers, e.g. [\"yes\", \"no\", \"dry-run only\"]. Advisory only — any free-text answer is still accepted when the session resumes."`
}

// AskQuestionError is the error the ask_question tool's Run returns to force
// fantasy's agent loop to stop the current turn instead of continuing it
// with a normal (successful) tool result.
//
// This type intentionally lives in package tools, not package agent: package
// agent already imports package tools (see coordinator.go's tool wiring), so
// package tools importing package agent back — to reuse agent.
// AwaitingAnswerError directly — would create an import cycle. agent.Run
// normalizes an AskQuestionError it sees coming back from fantasy's Stream()
// into the pre-existing agent.AwaitingAnswerError (see the errors.As check
// next to the peak-hours abort-err normalization in agent.go), so every
// downstream consumer (Finish-part text, `crush run --json`'s exit_reason,
// sessions why/diff, …) only ever has to know about the one agent-level
// type. This struct is the narrow carrier that crosses the package
// boundary.
type AskQuestionError struct {
	Question  string
	Options   []string
	SessionID string
}

func (e *AskQuestionError) Error() string {
	return fmt.Sprintf("agent asked a question and is awaiting an answer (session %s): %s", e.SessionID, e.Question)
}

// NewAskQuestionTool builds the ask_question agent tool. Run never returns a
// normal (successful) ToolResponse for a well-formed question: it always
// returns a non-nil *AskQuestionError as the Go error, which fantasy's
// executeSingleTool treats as a critical error and propagates all the way up
// as the agent loop's own error (see charm.land/fantasy's agent.go
// executeSingleTool: a non-nil error from a tool's Run aborts the step and
// is returned from Stream/Run verbatim). That is what lets agent.Run's
// error-classification chain (agent.go) catch it and force-finish the turn
// via AddFinish, exactly like the existing PeakHoursError path.
func NewAskQuestionTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AskQuestionToolName,
		askQuestionDescription,
		func(ctx context.Context, params AskQuestionParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			question := strings.TrimSpace(params.Question)
			if question == "" {
				// A malformed call (empty question) is a normal, retryable
				// tool error — NOT a real question — so it must NOT stop
				// the turn. Return it as an ordinary error ToolResponse
				// (nil Go error) so fantasy just reports it to the model
				// and lets the turn continue.
				return fantasy.NewTextErrorResponse("question must not be empty"), nil
			}

			sessionID := GetSessionFromContext(ctx)

			return fantasy.ToolResponse{}, &AskQuestionError{
				Question:  question,
				Options:   params.Options,
				SessionID: sessionID,
			}
		},
	)
}
