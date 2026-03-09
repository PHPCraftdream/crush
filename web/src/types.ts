// ─── Shared types mirroring Go structs ────────────────────────────────────────

export type TodoStatus = "pending" | "in_progress" | "completed";

export interface Todo {
  content: string;
  status: TodoStatus;
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
  CreatedAt: number;
  UpdatedAt: number;
  IsSummaryMessage: boolean;
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

export interface ConfigPayload {
  models?: Record<string, { Provider: string; Model: string }>;
  providers?: Record<string, { models?: { id: string; name: string }[] }>;
}

export interface LSPState {
  name: string;
  state: string;
  diagnosticCount: number;
}

export interface MCPState {
  servers: { name: string; status: string }[];
}

export interface AgentBusyPayload {
  SessionID: string;
  Busy: boolean;
}

// ─── WebSocket protocol ────────────────────────────────────────────────────────

export interface WSMessage<T = unknown> {
  id?: string;
  type: string;
  payload?: T;
  error?: string;
}
