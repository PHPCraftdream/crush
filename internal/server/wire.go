package server

import "github.com/charmbracelet/crush/internal/message"

// PartWire is the JSON wire format for a ContentPart sent to the browser.
// All fields use PascalCase to match the TypeScript interface names.
type PartWire struct {
	Type string `json:"type"`

	// text
	Text string `json:"Text,omitempty"`

	// thinking
	Thinking string `json:"Thinking,omitempty"`

	// tool_call
	ID       string `json:"ID,omitempty"`
	Name     string `json:"Name,omitempty"`
	Input    string `json:"Input,omitempty"`
	Finished bool   `json:"Finished,omitempty"`

	// tool_result
	ToolCallID string `json:"ToolCallID,omitempty"`
	Content    string `json:"Content,omitempty"`
	IsError    bool   `json:"IsError,omitempty"`
	Metadata   string `json:"Metadata,omitempty"`

	// finish
	Reason        string `json:"Reason,omitempty"`
	FinishMessage string `json:"Message,omitempty"`
	Details       string `json:"Details,omitempty"`
}

// MessageWire is the JSON wire format for a Message sent to the browser.
type MessageWire struct {
	ID               string     `json:"ID"`
	Role             string     `json:"Role"`
	SessionID        string     `json:"SessionID"`
	Parts            []PartWire `json:"Parts"`
	Model            string     `json:"Model"`
	Provider         string     `json:"Provider"`
	CreatedAt        int64      `json:"CreatedAt"`
	UpdatedAt        int64      `json:"UpdatedAt"`
	IsSummaryMessage bool       `json:"IsSummaryMessage"`
	Pinned           bool       `json:"Pinned"`
	Hidden           bool       `json:"Hidden"`
	AutoResumed      bool       `json:"AutoResumed"`
}

func toPartWire(part message.ContentPart) PartWire {
	switch p := part.(type) {
	case message.TextContent:
		return PartWire{Type: "text", Text: p.Text}
	case message.ReasoningContent:
		return PartWire{Type: "thinking", Thinking: p.Thinking}
	case message.ToolCall:
		return PartWire{Type: "tool_call", ID: p.ID, Name: p.Name, Input: p.Input, Finished: p.Finished}
	case message.ToolResult:
		return PartWire{Type: "tool_result", ToolCallID: p.ToolCallID, Name: p.Name, Content: p.Content, IsError: p.IsError, Metadata: p.Metadata}
	case message.Finish:
		return PartWire{Type: "finish", Reason: string(p.Reason), FinishMessage: p.Message, Details: p.Details}
	default:
		return PartWire{Type: "unknown"}
	}
}

func toMessageWire(m message.Message) MessageWire {
	parts := make([]PartWire, len(m.Parts))
	for i, p := range m.Parts {
		parts[i] = toPartWire(p)
	}
	return MessageWire{
		ID:               m.ID,
		Role:             string(m.Role),
		SessionID:        m.SessionID,
		Parts:            parts,
		Model:            m.Model,
		Provider:         m.Provider,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
		IsSummaryMessage: m.IsSummaryMessage,
		Pinned:           m.Pinned,
		Hidden:           m.Hidden,
		AutoResumed:      m.AutoResumed,
	}
}

func toMessagesWire(msgs []message.Message) []MessageWire {
	result := make([]MessageWire, len(msgs))
	for i, m := range msgs {
		result[i] = toMessageWire(m)
	}
	return result
}
