// ─── Shared types mirroring Go structs ────────────────────────────────────────

export type TodoStatus = "pending" | "in_progress" | "completed";

export interface Todo {
  content: string;
  status: TodoStatus;
  active_form?: string;
}

export interface Session {
  ID: string;
  ParentSessionID: string;
  Title: string;
  MessageCount: number;
  PromptTokens: number;
  CompletionTokens: number;
  SummaryMessageID: string;
  Cost: number;
  Todos: Todo[];
  CreatedAt: number;
  UpdatedAt: number;
  CWD: string;

  LargeModelProvider: string;
  LargeModelID: string;
  LargeModelReasoningEffort: string; // "low", "medium", "high", or "max"
  SmallModelProvider: string;
  SmallModelID: string;
  SmallModelReasoningEffort: string; // "low", "medium", "high", or "max"

  SystemPrompt: string;
  YoloEnabled: boolean;

  // Set by the server when another live crush process holds the session
  // lock (heartbeat fresher than ~20s). When true, the UI flips the
  // session into a read-only "follow" mode: input disabled, controls
  // hidden, banner shown, polling drives live updates instead of pubsub.
  OwnedExternal?: boolean;
  OwnedByPID?: number;
}

export type MessageRole = "user" | "assistant" | "tool" | "system";

export interface TextContent {
  type: "text";
  Text: string;
}

export interface ReasoningContent {
  type: "thinking";
  Thinking: string;
}

export interface ToolCall {
  type: "tool_call";
  ID: string;
  Name: string;
  Input: string;
  Finished: boolean;
}

export interface ToolResult {
  type: "tool_result";
  ToolCallID: string;
  Name: string;
  Content: string;
  IsError: boolean;
  Metadata?: string;
}

export interface FinishPart {
  type: "finish";
  Reason: string;
  Message: string;
  Details: string;
}

export type ContentPart =
  | TextContent
  | ReasoningContent
  | ToolCall
  | ToolResult
  | FinishPart;

export interface Message {
  ID: string;
  SessionID: string;
  Role: MessageRole;
  Parts: ContentPart[];
  Model: string;
  Provider: string;
  ReasoningEffort: string; // "low", "medium", "high", or "max" - for Claude models
  CreatedAt: number;
  UpdatedAt: number;
  IsSummaryMessage: boolean;
  Pinned: boolean;
  Hidden: boolean;
  AutoResumed: boolean;
  BackgroundJobNotice: boolean;
}

export interface PermissionRequest {
  ID: string;
  SessionID: string;
  ToolCallID: string;
  ToolName: string;
  Description: string;
  Action: string;
  Path: string;
  Params: unknown;
}

export interface PermissionRule {
  ID: string;
  SessionID: string;
  ToolName: string;
  Action: string;
  Path: string;
  CreatedAt: number;
  Enabled: boolean;
}

export interface PermissionNotification {
  ToolCallID: string;
  Granted: boolean;
  Denied: boolean;
}

export interface ModelInfo {
  id: string;
  name: string;
  provider: string;
}

export interface ProviderInfo {
  name?: string;
  enabled?: boolean;
  type?: string;
  models?: { id: string; name: string; contextWindow?: number }[];
  baseUrl?: string;
  isCustom?: boolean;
  apiKeySet?: boolean;
}

export interface ConfigPayload {
  models?: Record<string, { Provider: string; Model: string }>;
  providers?: Record<string, ProviderInfo>;
  yolo?: boolean;
  debug?: boolean;
  debugLsp?: boolean;
  theme?: string;
  recentLargeModels?: Array<{ Provider: string; Model: string }>;
  recentSmallModels?: Array<{ Provider: string; Model: string }>;
  contextPaths?: string[];
  skillsPaths?: string[];
  initializeAs?: string;
  version?: string;
  cwd?: string;
  // Server-resolved keep-alive preference: backend defaults nil → true and
  // always sends an explicit bool, so this is non-nullable in practice
  // (kept optional only to survive transitional reloads against an older
  // server build).
  keepAliveEnabled?: boolean;
}

export interface SkillInfo {
  name: string;
  description: string;
  path: string;
  source?: string;
  instructions?: string;
}

export interface SkillsSnapshot {
  skills: SkillInfo[];
  paths: string[];
}

export interface MCPServerInfo {
  name: string;
  status: string;
  disabled: boolean;
  toolCount: number;
  tools?: string[];
  serverType?: string;
  command?: string;
  args?: string[];
  url?: string;
  env?: Record<string, string>;
  headers?: Record<string, string>;
  source?: string;
}

export interface MCPState {
  servers: MCPServerInfo[];
}

export interface AgentBusyPayload {
  SessionID: string;
  Busy: boolean;
}

export interface SummarizeQueuedPayload {
  SessionID: string;
  Queued: boolean;
}



// ─── WebSocket protocol ────────────────────────────────────────────────────────

export interface WSMessage<T = unknown> {
  id?: string;
  type: string;
  payload?: T;
  error?: string;
}
