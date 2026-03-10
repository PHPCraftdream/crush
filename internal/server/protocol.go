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
	EventSkills                  = "skills"
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
	CmdUpdateMessageThinking        = "update_message_thinking"
	CmdGetSystemPrompt              = "get_system_prompt"
	CmdSetSystemPrompt              = "set_system_prompt"
	CmdSummarizeSession             = "summarize_session"
	CmdDeleteMessagePart            = "delete_message_part"
	CmdUpdateMessagePart            = "update_message_part"
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

// New command constants for settings management.
const (
	CmdSetDebug             = "set_debug"
	CmdAddContextPath       = "add_context_path"
	CmdRemoveContextPath    = "remove_context_path"
	CmdGetSkills            = "get_skills"
	CmdAddSkillsPath        = "add_skills_path"
	CmdRemoveSkillsPath     = "remove_skills_path"
	CmdInitializeProject    = "initialize_project"
	CmdAddCustomProvider    = "add_custom_provider"
	CmdRemoveCustomProvider = "remove_custom_provider"
	CmdUpdateCustomProvider = "update_custom_provider"
	CmdUpdateTodos          = "update_todos"
)

// SetDebugPayload controls debug logging options.
type SetDebugPayload struct {
	Debug    bool `json:"debug"`
	DebugLSP bool `json:"debugLsp"`
}

// AddContextPathPayload adds a path to options.context_paths.
type AddContextPathPayload struct {
	Path string `json:"path"`
}

// RemoveContextPathPayload removes a path from options.context_paths.
type RemoveContextPathPayload struct {
	Path string `json:"path"`
}

// AddSkillsPathPayload adds a path to options.skills_paths.
type AddSkillsPathPayload struct {
	Path string `json:"path"`
}

// RemoveSkillsPathPayload removes a path from options.skills_paths.
type RemoveSkillsPathPayload struct {
	Path string `json:"path"`
}

// SkillInfo is the wire format for a single discovered skill.
type SkillInfo struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Path         string `json:"path"`
	Source       string `json:"source,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// SkillsSnapshot is the full skills state.
type SkillsSnapshot struct {
	Skills []SkillInfo `json:"skills"`
	Paths  []string    `json:"paths"`
}

// CustomModelPayload carries a model definition for custom providers.
type CustomModelPayload struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	ContextWindow int64   `json:"contextWindow,omitempty"`
	CostPer1MIn   float64 `json:"costPer1mIn,omitempty"`
	CostPer1MOut  float64 `json:"costPer1mOut,omitempty"`
}

// AddCustomProviderPayload adds a fully custom provider.
type AddCustomProviderPayload struct {
	ID      string               `json:"id"`
	Name    string               `json:"name,omitempty"`
	Type    string               `json:"type"`
	BaseURL string               `json:"baseUrl"`
	APIKey  string               `json:"apiKey,omitempty"`
	Models  []CustomModelPayload `json:"models,omitempty"`
}

// RemoveCustomProviderPayload removes a custom provider by id.
type RemoveCustomProviderPayload struct {
	ID string `json:"id"`
}

// UpdateCustomProviderPayload updates an existing custom provider.
type UpdateCustomProviderPayload struct {
	OldID   string               `json:"oldId"`
	ID      string               `json:"id"`
	Name    string               `json:"name,omitempty"`
	Type    string               `json:"type"`
	BaseURL string               `json:"baseUrl"`
	APIKey  string               `json:"apiKey,omitempty"`
	Models  []CustomModelPayload `json:"models,omitempty"`
}

// MCPServerInfo is the wire format for a single MCP server state.
type MCPServerInfo struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Disabled   bool              `json:"disabled"`
	ToolCount  int               `json:"toolCount"`
	Tools      []string          `json:"tools,omitempty"`
	ServerType string            `json:"serverType,omitempty"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	URL        string            `json:"url,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

// MCPSnapshot is the full MCP state broadcast to clients.
type MCPSnapshot struct {
	Servers []MCPServerInfo `json:"servers"`
}

const (
	CmdSetYolo           = "set_yolo"
	CmdSetProviderKey    = "set_provider_key"
	CmdRemoveProviderKey = "remove_provider_key"
	CmdLogClientEvent    = "log_client_event"
	CmdLogClientError    = "log_client_error"
	CmdSetMCPDisabled    = "set_mcp_disabled"
	CmdAddMCPServer      = "add_mcp_server"
	CmdRemoveMCPServer   = "remove_mcp_server"
	CmdUpdateMCPServer   = "update_mcp_server"
	CmdSetLSPDisabled    = "set_lsp_disabled"
	CmdAddLSPServer      = "add_lsp_server"
	CmdRemoveLSPServer   = "remove_lsp_server"
	CmdUpdateLSPServer   = "update_lsp_server"
)

// LSPServerInfo is the wire format for a single LSP server state.
type LSPServerInfo struct {
	Name            string            `json:"name"`
	State           string            `json:"state"`
	Disabled        bool              `json:"disabled"`
	DiagnosticCount int               `json:"diagnosticCount"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	FileTypes       []string          `json:"fileTypes,omitempty"`
}

// LSPSnapshot is the full LSP state broadcast to clients.
type LSPSnapshot struct {
	Servers []LSPServerInfo `json:"servers"`
}

type SetLSPDisabledPayload struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

type AddLSPServerPayload struct {
	Name      string            `json:"name"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	FileTypes []string          `json:"fileTypes,omitempty"`
	Timeout   int               `json:"timeout,omitempty"`
}

type RemoveLSPServerPayload struct {
	Name string `json:"name"`
}

type UpdateLSPServerPayload struct {
	OldName   string            `json:"oldName"`
	Name      string            `json:"name"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	FileTypes []string          `json:"fileTypes,omitempty"`
	Timeout   int               `json:"timeout,omitempty"`
}

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

type UpdateMessageThinkingPayload struct {
	MessageID string `json:"messageID"`
	Thinking  string `json:"thinking"`
}

// ConfigWire is the frontend-facing config payload with PascalCase field names
// matching the TypeScript ConfigPayload type.
type ConfigWire struct {
	Models            map[string]ModelEntryWire `json:"models"`
	Providers         map[string]ProviderWire   `json:"providers"`
	Yolo              bool                      `json:"yolo"`
	Debug             bool                      `json:"debug"`
	DebugLSP          bool                      `json:"debugLsp"`
	Theme             string                    `json:"theme"`
	RecentLargeModels []ModelEntryWire          `json:"recentLargeModels,omitempty"`
	RecentSmallModels []ModelEntryWire          `json:"recentSmallModels,omitempty"`
	ContextPaths      []string                  `json:"contextPaths,omitempty"`
	SkillsPaths       []string                  `json:"skillsPaths,omitempty"`
	InitializeAs      string                    `json:"initializeAs,omitempty"`
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
	BaseURL   string          `json:"baseUrl,omitempty"`
	IsCustom  bool            `json:"isCustom,omitempty"`
	APIKeySet bool            `json:"apiKeySet,omitempty"`
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

type SummarizeSessionPayload struct {
	SessionID string `json:"sessionID"`
}

type LogClientEventPayload struct {
	Event   string         `json:"event"`
	Details map[string]any `json:"details,omitempty"`
}

type LogClientErrorPayload struct {
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
	Stack   string `json:"stack,omitempty"`
}

type DeleteMessagePartPayload struct {
	MessageID string `json:"messageID"`
	PartIndex int    `json:"partIndex"`
}

type UpdateMessagePartPayload struct {
	MessageID string `json:"messageID"`
	PartIndex int    `json:"partIndex"`
	Content   string `json:"content"`
}

type SetMCPDisabledPayload struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

// AddMCPServerPayload carries a new MCP server definition.
// The JSON format mirrors MCPConfig with an extra "name" field.
type AddMCPServerPayload struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

type RemoveMCPServerPayload struct {
	Name string `json:"name"`
}

// UpdateMCPServerPayload updates an existing MCP server config by removing and re-adding it.
// OldName identifies the server; other fields are the new config.
type UpdateMCPServerPayload struct {
	OldName string            `json:"oldName"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

// UpdateTodosPayload replaces the todo list for a session.
type UpdateTodosPayload struct {
	SessionID string    `json:"sessionID"`
	Todos     []TodoWire `json:"todos"`
}

// TodoWire mirrors session.Todo for the WebSocket protocol.
type TodoWire struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"`
}
