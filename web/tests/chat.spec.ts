import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Input state ────────────────────────────────────────────────────────────

test("message input is disabled without active session", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByPlaceholder("Select or create a session")).toBeDisabled();
});

test("Send button is disabled without active session", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeDisabled();
});

test("message input enables after selecting a session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "inp-1", Title: "Input Session" })],
  });
  await expect(page.getByText("Input Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Input Session").first().click();
  await expect(
    page.getByPlaceholder("Message… (Enter to send)")
  ).toBeEnabled({ timeout: 2000 });
});

test("Send button enables when text entered with active session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "btn-1", Title: "Btn Session" })],
  });
  await expect(page.getByText("Btn Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Btn Session").first().click();
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });
  await page.getByPlaceholder("Message… (Enter to send)").fill("hello");
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeEnabled({ timeout: 2000 });
});

test("Send button stays disabled with only whitespace", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ws-1", Title: "WS Session" })],
  });
  await expect(page.getByText("WS Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("WS Session").first().click();
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });
  await page.getByPlaceholder("Message… (Enter to send)").fill("   ");
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeDisabled();
});

// ── Messages rendering ─────────────────────────────────────────────────────

test("user message appears in chat area", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "msg-1", Title: "User Msg Session" })],
  });
  await expect(page.getByText("User Msg Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("User Msg Session").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "u1",
        Role: "user",
        Parts: [{ type: "text", Text: "Hello from user" }],
      }),
    ],
  });
  await expect(page.getByText("Hello from user")).toBeVisible({ timeout: 2000 });
});

test("assistant message appears in chat area", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "asst-1", Title: "Asst Msg Session" })],
  });
  await expect(page.getByText("Asst Msg Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Asst Msg Session").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "a1",
        Role: "assistant",
        Parts: [{ type: "text", Text: "I am the assistant" }],
      }),
    ],
  });
  await expect(page.getByText("I am the assistant")).toBeVisible({ timeout: 2000 });
});

test("message_created event shows new message immediately", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "live-1", Title: "Live Chat" })],
  });
  await expect(page.getByText("Live Chat").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Live Chat").first().click();
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "live-m1",
      SessionID: "live-1",
      Role: "user",
      Parts: [{ type: "text", Text: "Live user message" }],
    }),
  });
  await expect(page.getByText("Live user message")).toBeVisible({ timeout: 2000 });
});

test("message_updated event updates message content", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "upd-1", Title: "Update Chat" })],
  });
  await expect(page.getByText("Update Chat").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Update Chat").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "upd-m1",
        SessionID: "upd-1",
        Role: "assistant",
        Parts: [{ type: "text", Text: "Partial..." }],
      }),
    ],
  });
  await expect(page.getByText("Partial...")).toBeVisible({ timeout: 2000 });

  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: makeMessage({
      ID: "upd-m1",
      SessionID: "upd-1",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Complete response" }],
    }),
  });
  await expect(page.getByText("Complete response")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Partial...")).not.toBeVisible();
});

test("tool_call part renders tool name", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "tc-1", Title: "Tool Chat" })],
  });
  await expect(page.getByText("Tool Chat").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Tool Chat").first().click();
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "tc-m1",
        Role: "assistant",
        Parts: [
          { type: "tool_call", ID: "call-1", Name: "bash", Input: "ls -la", Finished: false },
        ],
      }),
    ],
  });
  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
});

test("tool_result part renders result content", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "tr-1", Title: "Result Chat" })],
  });
  await expect(page.getByText("Result Chat").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Result Chat").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "tr-m1",
        Role: "tool",
        Parts: [
          {
            type: "tool_result",
            ToolCallID: "call-1",
            Name: "bash",
            Content: "total 42\ndrwxr-xr-x 5 user user",
            IsError: false,
          },
        ],
      }),
    ],
  });
  await expect(page.getByText("total 42")).toBeVisible({ timeout: 2000 });
});

// ── Busy state ─────────────────────────────────────────────────────────────

test("typing indicator appears when session is busy", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "busy-1", Title: "Busy Session" })],
  });
  await expect(page.getByText("Busy Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Busy Session").first().click();
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "busy-1", Busy: true },
  });
  await expect(
    page.locator(".animate-pulse-dots").first()
  ).toBeVisible({ timeout: 2000 });
});

test("Stop button replaces Send when session is busy", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "stop-1", Title: "Stop Me" })],
  });
  await expect(page.getByText("Stop Me").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Stop Me").first().click();
  // Wait for session to be active before sending busy event
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "stop-1", Busy: true },
  });
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeVisible({ timeout: 3000 });
  await expect(page.getByRole("button", { name: "Send", exact: true })).not.toBeVisible();
});

test("typing indicator disappears when session is no longer busy", async ({
  page,
}) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "done-1", Title: "Done Session" })],
  });
  await expect(page.getByText("Done Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Done Session").first().click();
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "done-1", Busy: true },
  });
  await expect(page.locator(".animate-pulse-dots").first()).toBeVisible({ timeout: 2000 });

  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "done-1", Busy: false },
  });
  await expect(page.locator(".animate-pulse-dots").first()).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeVisible();
});

// ── Keyboard ───────────────────────────────────────────────────────────────

test("Shift+Enter inserts newline instead of sending", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "key-1", Title: "Key Session" })],
  });
  await expect(page.getByText("Key Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Key Session").first().click();
  const textarea = page.getByPlaceholder("Message… (Enter to send)");
  await textarea.fill("line one");
  await textarea.press("Shift+Enter");
  await textarea.type("line two");
  // Value should contain a newline
  const val = await textarea.inputValue();
  expect(val).toContain("\n");
});

// ── Send message command ────────────────────────────────────────────────────

test("Enter sends send_message command with sessionID and content", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "send-1", Title: "Send Test" })],
  });
  await expect(page.getByText("Send Test").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Send Test").first().click();
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });
  await page.getByPlaceholder("Message… (Enter to send)").fill("hello server");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  const cmd = await waitForWSSend(page, "send_message");
  expect((cmd.payload as { sessionID: string; content: string }).sessionID).toBe("send-1");
  expect((cmd.payload as { sessionID: string; content: string }).content).toBe("hello server");
});

test("textarea clears after send", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "clr-1", Title: "Clear Test" })],
  });
  await expect(page.getByText("Clear Test").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Clear Test").first().click();
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });
  await page.getByPlaceholder("Message… (Enter to send)").fill("will be cleared");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toHaveValue("", { timeout: 2000 });
});

test("Stop button sends cancel_agent command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "canc-1", Title: "Cancel Session" })],
  });
  await expect(page.getByText("Cancel Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Cancel Session").first().click();
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "canc-1", Busy: true },
  });
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  const cmd = await waitForWSSend(page, "cancel_agent");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("canc-1");
});
