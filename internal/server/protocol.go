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
	EventMessageCreated         = "message_created"
	EventMessageUpdated         = "message_updated"
	EventMessageDeleted         = "message_deleted"
	EventSessionCreated         = "session_created"
	EventSessionUpdated         = "session_updated"
	EventSessionDeleted         = "session_deleted"
	EventFileUpdated            = "file_updated"
	EventMCPState               = "mcp_state"
	EventLSPState               = "lsp_state"
	EventAgentBusy              = "agent_busy"
	EventSessionsList           = "sessions_list"
	EventMessagesList           = "messages_list"
	EventConfig                 = "config"
	EventLogs                   = "logs"
	EventResponse               = "response"
	EventError                  = "error"
	EventSystemPrompt           = "system_prompt"
	EventSkills                 = "skills"
	// EventSummarizeQueued is sent when a manual summarise is queued (busy=true)
	// or dequeued/completed (busy=false) for a session.
	EventSummarizeQueued = "summarize_queued"
)

// Inbound command types (client → server).
const (
	CmdSendMessage               = "send_message"
	CmdInterruptAndSend          = "interrupt_and_send"
	CmdInjectMessage             = "inject_message"
	CmdCancelAgent               = "cancel_agent"
	CmdCreateSession             = "create_session"
	CmdForkSession               = "fork_session"
	CmdDeleteSession             = "delete_session"
	CmdDeleteOtherSessions       = "delete_other_sessions"
	CmdListSessions              = "list_sessions"
	CmdLoadMessages              = "load_messages"
	CmdGetConfig                 = "get_config"
	CmdGetLogs                   = "get_logs"
	CmdSetTheme                  = "set_theme"
	CmdSetKeepAlive              = "set_keep_alive"
	CmdRenameSession             = "rename_session"
	CmdSetSessionModels          = "set_session_models"
	CmdRemoveRecentModel         = "remove_recent_model"
	CmdTrackModelUsage           = "track_model_usage"
	CmdDeleteMessage             = "delete_message"
	CmdDeleteMessages            = "delete_messages"
	CmdUpdateMessageContent      = "update_message_content"
	CmdUpdateMessageThinking     = "update_message_thinking"
	CmdGetSystemPrompt           = "get_system_prompt"
	CmdSetSystemPrompt           = "set_system_prompt"
	CmdSummarizeSession          = "summarize_session"
	CmdCancelQueuedSummarize     = "cancel_queued_summarize"
	CmdDeleteMessagePart         = "delete_message_part"
	CmdUpdateMessagePart         = "update_message_part"
	CmdTogglePinMessage          = "toggle_pin_message"
	CmdRerunMessage              = "rerun_message"
)

// Payload structs for inbound commands.

// Inbound payload structs — json tags use camelCase to match the JS client.

// ModelOverrideWire carries per-call model overrides from the client.
type ModelOverrideWire struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"` // "low", "medium", "high", or "max"
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

type ForkSessionPayload struct {
	SessionID string `json:"sessionID"`
	Title     string `json:"title"`
}

type DeleteSessionPayload struct {
	SessionID string `json:"sessionID"`
}

// DeleteOtherSessionsPayload asks the server to delete every top-level
// session except the one identified by KeepID.
type DeleteOtherSessionsPayload struct {
	KeepID string `json:"keepID"`
}

type LoadMessagesPayload struct {
	SessionID string `json:"sessionID"`
}

type SetThemePayload struct {
	Theme string `json:"theme"` // "dark" or "light"
}

type SetKeepAlivePayload struct {
	Enabled bool `json:"enabled"`
}

type RenameSessionPayload struct {
	SessionID string `json:"sessionID"`
	Title     string `json:"title"`
}

type RerunMessagePayload struct {
	MessageID string `json:"messageID"`
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
	CmdSetProviderPeakHours = "set_provider_peak_hours"
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

// PeakHoursWirePayload is the wire shape of config.PeakHoursWindow. It
// uses the same lowerCamelCase JSON keys ("start"/"end") as the config
// type so the WS layer can marshal/unmarshal it directly without a
// remapping step.
type PeakHoursWirePayload struct {
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

// AddCustomProviderPayload adds a fully custom provider.
//
// Scope is "global" or "local" (workspace); empty defaults to "global",
// matching every scope-aware CLI command's default (crush providers,
// crush mcp, crush claude-init, ...).
type AddCustomProviderPayload struct {
	ID        string                `json:"id"`
	Name      string                `json:"name,omitempty"`
	Type      string                `json:"type"`
	BaseURL   string                `json:"baseUrl"`
	APIKey    string                `json:"apiKey,omitempty"`
	Models    []CustomModelPayload  `json:"models,omitempty"`
	PeakHours *PeakHoursWirePayload `json:"peakHours,omitempty"`
	Scope     string                `json:"scope,omitempty"`
}

// RemoveCustomProviderPayload removes a custom provider by id.
//
// Scope is "global" or "local" (workspace); empty defaults to "global" —
// must match the scope the provider was actually added under, or the
// override won't be found and removal is a silent no-op.
type RemoveCustomProviderPayload struct {
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"`
}

// UpdateCustomProviderPayload updates an existing custom provider.
// Update is a full replace, not a partial merge — mirroring APIKey and
// every other field, PeakHours is taken verbatim from the payload:
// present → set (validated), absent → cleared to nil.
//
// Scope is "global" or "local" (workspace); empty defaults to "global".
type UpdateCustomProviderPayload struct {
	OldID     string                `json:"oldId"`
	ID        string                `json:"id"`
	Name      string                `json:"name,omitempty"`
	Type      string                `json:"type"`
	BaseURL   string                `json:"baseUrl"`
	APIKey    string                `json:"apiKey,omitempty"`
	Models    []CustomModelPayload  `json:"models,omitempty"`
	PeakHours *PeakHoursWirePayload `json:"peakHours,omitempty"`
	Scope     string                `json:"scope,omitempty"`
}

// SetProviderPeakHoursPayload sets or clears ONLY the peak_hours field on
// ANY provider (built-in/catwalk-known or custom) — a targeted single-field
// write, unlike Add/UpdateCustomProviderPayload which replace every field
// and are therefore only safe to use on custom providers the client fully
// owns. This mirrors what `crush providers set <id> --peak-hours` does on
// the CLI side.
//
// PeakHours nil/absent clears the window. Scope is "global" or "local"
// (workspace); empty defaults to "global".
type SetProviderPeakHoursPayload struct {
	ID        string                `json:"id"`
	PeakHours *PeakHoursWirePayload `json:"peakHours,omitempty"`
	Scope     string                `json:"scope,omitempty"`
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
	Source     string            `json:"source,omitempty"`
}

// MCPSnapshot is the full MCP state broadcast to clients.
type MCPSnapshot struct {
	Servers []MCPServerInfo `json:"servers"`
}

const (
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

type TrackModelUsagePayload struct {
	ModelType string `json:"modelType"` // "large" or "small"
	Provider  string `json:"provider"`
	Model     string `json:"model"`
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
	Debug             bool                      `json:"debug"`
	DebugLSP          bool                      `json:"debugLsp"`
	Theme             string                    `json:"theme"`
	RecentLargeModels []ModelEntryWire          `json:"recentLargeModels,omitempty"`
	RecentSmallModels []ModelEntryWire          `json:"recentSmallModels,omitempty"`
	ContextPaths      []string                  `json:"contextPaths,omitempty"`
	SkillsPaths       []string                  `json:"skillsPaths,omitempty"`
	InitializeAs      string                    `json:"initializeAs,omitempty"`
	Version           string                    `json:"version,omitempty"`
	CWD               string                    `json:"cwd,omitempty"`
	// KeepAliveEnabled mirrors Options.KeepAliveEnabled with the default
	// resolved server-side (nil → true), so the frontend never sees an
	// ambiguous undefined.
	KeepAliveEnabled bool `json:"keepAliveEnabled"`
}

// ModelEntryWire represents a selected model entry (large/small/etc).
type ModelEntryWire struct {
	Provider string `json:"Provider"`
	Model    string `json:"Model"`
}

// ProviderWire is a provider with its available models.
type ProviderWire struct {
	Name      string                `json:"name,omitempty"`
	Enabled   bool                  `json:"enabled"`
	Type      string                `json:"type,omitempty"`
	Models    []ModelInfoWire       `json:"models,omitempty"`
	BaseURL   string                `json:"baseUrl,omitempty"`
	IsCustom  bool                  `json:"isCustom,omitempty"`
	APIKeySet bool                  `json:"apiKeySet,omitempty"`
	PeakHours *PeakHoursWirePayload `json:"peakHours,omitempty"`
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

type GetLogsPayload struct {
	Lines int `json:"lines,omitempty"` // number of lines from end, 0 for all
}

type SetSystemPromptPayload struct {
	SessionID string `json:"sessionID"`
	Content   string `json:"content"`
}

type SummarizeSessionPayload struct {
	SessionID string `json:"sessionID"`
}

// SummarizeQueuedPayload is sent when a manual compact is queued or dequeued.
type SummarizeQueuedPayload struct {
	SessionID string `json:"SessionID"`
	Queued    bool   `json:"Queued"`
}

type CancelQueuedSummarizePayload struct {
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

type TogglePinMessagePayload struct {
	MessageID string `json:"messageID"`
	Pinned    bool   `json:"pinned"`
}

type SetMCPDisabledPayload struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

// AddMCPServerPayload carries a new MCP server definition.
// The JSON format mirrors MCPConfig with an extra "name" field.
type AddMCPServerPayload struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
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
	SessionID string     `json:"sessionID"`
	Todos     []TodoWire `json:"todos"`
}

// TodoWire mirrors session.Todo for the WebSocket protocol.
type TodoWire struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"`
}
