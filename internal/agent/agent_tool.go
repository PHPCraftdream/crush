package agent

import (
	"context"
	_ "embed"
	"errors"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform. When resume_session_id is set, this is instead the answer/instruction to send to the paused sub-agent — it is appended to that sub-agent's existing conversation, not a new task."`
	// ResumeSessionID, when set, continues an existing sub-agent session
	// instead of starting a new one. Use this to answer a sub-agent that
	// stopped with a "SUB-AGENT QUESTION" tool result: the sub-agent's full
	// prior context (files it already read, work already done) is preserved,
	// so the task does not have to be restarted from scratch. Must be the
	// exact session id from that tool result, and must be a child of the
	// current session — resuming any other session id is rejected.
	ResumeSessionID string `json:"resume_session_id,omitempty" description:"Optional: the child session id of a paused sub-agent to resume (from a prior \"SUB-AGENT QUESTION (session ...)\" result), instead of starting a new sub-agent. The sub-agent's context is preserved."`
}

const (
	AgentToolName = "agent"
)

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	prompt, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, true)
	if err != nil {
		return nil, err
	}
	return fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			return c.runSubAgent(ctx, subAgentParams{
				Agent:           agent,
				SessionID:       sessionID,
				AgentMessageID:  agentMessageID,
				ToolCallID:      call.ID,
				Prompt:          params.Prompt,
				SessionTitle:    "New Agent Session",
				ResumeSessionID: params.ResumeSessionID,
			})
		},
	), nil
}
