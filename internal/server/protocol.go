package server

import "encoding/json"

// WSMessage is the envelope for all WebSocket messages in both directions.
type WSMessage struct {
	// ID is an optional correlation ID; set by the client on queries,
	// echoed back by the server in the paired response.
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Outbound event types (server → client).
const (
	EventMessageCreated          = "message_created"
	EventMessageUpdated          = "message_updated"
	EventMessageDeleted          = "message_deleted"
	EventSessionCreated          = "session_created"
	EventSessionUpdated          = "session_updated"
	EventSessionDeleted          = "session_deleted"
	EventPermissionRequest       = "permission_request"
	EventPermissionNotification  = "permission_notification"
	EventFileUpdated             = "file_updated"
	EventMCPState                = "mcp_state"
	EventLSPState                = "lsp_state"
	EventAgentBusy               = "agent_busy"
	EventSessionsList            = "sessions_list"
	EventMessagesList            = "messages_list"
	EventConfig                  = "config"
	EventResponse                = "response"
	EventError                   = "error"
	EventSystemPrompt            = "system_prompt"
)

// Inbound command types (client → server).
const (
	CmdSendMessage                  = "send_message"
	CmdCancelAgent                  = "cancel_agent"
	CmdCreateSession                = "create_session"
	CmdDeleteSession                = "delete_session"
	CmdListSessions                 = "list_sessions"
	CmdLoadMessages                 = "load_messages"
	CmdGrantPermission              = "grant_permission"
	CmdGrantPermissionPersistent    = "grant_permission_persistent"
	CmdDenyPermission               = "deny_permission"
	CmdGetConfig                    = "get_config"
	CmdSetTheme                     = "set_theme"
	CmdRenameSession                = "rename_session"
	CmdSetSessionModels             = "set_session_models"
	CmdRemoveRecentModel            = "remove_recent_model"
	CmdDeleteMessage                = "delete_message"
	CmdDeleteMessages               = "delete_messages"
	CmdUpdateMessageContent         = "update_message_content"
	CmdGetSystemPrompt              = "get_system_prompt"
	CmdSetSystemPrompt              = "set_system_prompt"
)

// Payload structs for inbound commands.

// Inbound payload structs — json tags use camelCase to match the JS client.

// ModelOverrideWire carries per-call model overrides from the client.
type ModelOverrideWire struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type SetSessionModelsPayload struct {
	SessionID  string             `json:"sessionID"`
	LargeModel *ModelOverrideWire `json:"largeModel"`
	SmallModel *ModelOverrideWire `json:"smallModel"`
}

type SendMessagePayload struct {
	SessionID   string `json:"sessionID"`
	Content     string `json:"content"`
	Attachments []struct {
		FileName string `json:"fileName"`
		MimeType string `json:"mimeType"`
		Data     []byte `json:"data"`
	} `json:"attachments,omitempty"`
	// Optional per-call model overrides. When absent the global config is used.
	LargeModel *ModelOverrideWire `json:"largeModel,omitempty"`
	SmallModel *ModelOverrideWire `json:"smallModel,omitempty"`
}

type CancelAgentPayload struct {
	SessionID string `json:"sessionID"`
}

type CreateSessionPayload struct {
	Title string `json:"title"`
}

type DeleteSessionPayload struct {
	SessionID string `json:"sessionID"`
}

type LoadMessagesPayload struct {
	SessionID string `json:"sessionID"`
}

// PermissionResponsePayload is sent by the client when granting or denying a permission.
type PermissionResponsePayload struct {
	PermissionID string `json:"permissionID"`
}

type SetThemePayload struct {
	Theme string `json:"theme"` // "dark" or "light"
}

type RenameSessionPayload struct {
	SessionID string `json:"sessionID"`
	Title     string `json:"title"`
}

// AgentBusyPayload is sent server→client; PascalCase matches other data structs.
type AgentBusyPayload struct {
	SessionID string
	Busy      bool
}

// MCPServerInfo is the wire format for a single MCP server state.
type MCPServerInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// MCPSnapshot is the full MCP state broadcast to clients.
type MCPSnapshot struct {
	Servers []MCPServerInfo `json:"servers"`
}

const (
	CmdSetYolo          = "set_yolo"
	CmdSetProviderKey   = "set_provider_key"
	CmdRemoveProviderKey = "remove_provider_key"
)

type RemoveRecentModelPayload struct {
	ModelType string `json:"modelType"` // "large" or "small"
	Provider  string `json:"provider"`
	Model     string `json:"model"`
}

type SetYoloPayload struct {
	Enabled bool `json:"enabled"`
}

type SetProviderKeyPayload struct {
	ProviderID string `json:"providerID"`
	APIKey     string `json:"apiKey"`
}

type RemoveProviderKeyPayload struct {
	ProviderID string `json:"providerID"`
}

type DeleteMessagePayload struct {
	MessageID string `json:"messageID"`
}

type DeleteMessagesPayload struct {
	MessageIDs []string `json:"messageIDs"`
}

type UpdateMessageContentPayload struct {
	MessageID string `json:"messageID"`
	Content   string `json:"content"`
}

// ConfigWire is the frontend-facing config payload with PascalCase field names
// matching the TypeScript ConfigPayload type.
type ConfigWire struct {
	Models             map[string]ModelEntryWire   `json:"models"`
	Providers          map[string]ProviderWire     `json:"providers"`
	Yolo               bool                        `json:"yolo"`
	RecentLargeModels  []ModelEntryWire            `json:"recentLargeModels,omitempty"`
	RecentSmallModels  []ModelEntryWire            `json:"recentSmallModels,omitempty"`
}

// ModelEntryWire represents a selected model entry (large/small/etc).
type ModelEntryWire struct {
	Provider string `json:"Provider"`
	Model    string `json:"Model"`
}

// ProviderWire is a provider with its available models.
type ProviderWire struct {
	Name      string          `json:"name,omitempty"`
	Enabled   bool            `json:"enabled"`
	Type      string          `json:"type,omitempty"`
	Models    []ModelInfoWire `json:"models,omitempty"`
}

// ModelInfoWire is a single available model from a provider.
type ModelInfoWire struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
}

type GetSystemPromptPayload struct {
	SessionID string `json:"sessionID"`
}

type SetSystemPromptPayload struct {
	SessionID string `json:"sessionID"`
	Content   string `json:"content"`
}
