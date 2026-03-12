/** Shared test data factories */

export function makeSession(overrides: Record<string, unknown> = {}) {
  return {
    ID: "sess-1",
    Title: "Test Session",
    MessageCount: 0,
    PromptTokens: 0,
    CompletionTokens: 0,
    Cost: 0,
    Todos: [],
    CreatedAt: 1700000000000,
    UpdatedAt: 1700000000000,
    ParentSessionID: "",
    YoloEnabled: false,
    ...overrides,
  };
}

/** Wire format: Parts use {type, Text/Thinking/...} with PascalCase */
export function makeMessage(overrides: Record<string, unknown> = {}) {
  return {
    ID: "msg-1",
    SessionID: "sess-1",
    Role: "user" as const,
    Parts: [{ type: "text", Text: "Hello" }],
    Model: "",
    Provider: "",
    CreatedAt: 1700000000000,
    UpdatedAt: 1700000000000,
    IsSummaryMessage: false,
    ...overrides,
  };
}

export function makeConfig(overrides: Record<string, unknown> = {}) {
  return {
    models: {
      large: { Provider: "anthropic", Model: "claude-opus-4" },
      small: { Provider: "anthropic", Model: "claude-haiku-4" },
    },
    providers: {
      anthropic: {
        enabled: true,
        models: [
          { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
          { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
        ],
      },
    },
    yolo: false,
    ...overrides,
  };
}

export function makePermission(overrides: Record<string, unknown> = {}) {
  return {
    ID: "perm-1",
    SessionID: "sess-1",
    ToolCallID: "tc-1",
    ToolName: "bash",
    Description: "Run a shell command",
    Action: "execute",
    Path: "/tmp/script.sh",
    Params: {},
    ...overrides,
  };
}
