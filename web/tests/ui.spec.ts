import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Header ─────────────────────────────────────────────────────────────────

test("header shows session title when session is active", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "hdr-1", Title: "My Project Chat" })],
  });
  await expect(page.getByText("My Project Chat").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("My Project Chat").first().click();
  await expect(
    page.locator("header h1").getByText("My Project Chat")
  ).toBeVisible({ timeout: 2000 });
});

test("header shows default title when no session selected", async ({ page }) => {
  await page.goto("/");
  await expect(
    page.locator("header h1").getByText("No session selected")
  ).toBeVisible({ timeout: 2000 });
});

test("header shows model name from config", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: { models: { large: { Provider: "anthropic", Model: "claude-3-5-sonnet" } } },
  });
  await expect(page.getByText("claude-3-5-sonnet").first()).toBeVisible({ timeout: 2000 });
});

test("header shows token usage for active session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "tok-1", Title: "Token Session", PromptTokens: 1200, CompletionTokens: 800 })],
  });
  await expect(page.getByText("Token Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("Token Session").click();
  // 1200 + 800 = 2000 → "2.0k tok"
  await expect(page.getByTitle("Total tokens used")).toBeVisible({ timeout: 2000 });
});

test("header shows busy dots when agent is working", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "hdr-busy", Title: "Busy Header" })],
  });
  await expect(page.getByText("Busy Header")).toBeVisible({ timeout: 3000 });
  await page.getByText("Busy Header").click();
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "hdr-busy", Busy: true },
  });
  await expect(
    page.locator("header [title='Agent is working…']")
  ).toBeVisible({ timeout: 2000 });
});

// ── Settings panel ─────────────────────────────────────────────────────────

test("settings panel opens on gear click", async ({ page }) => {
  await page.goto("/");
  await page.getByTitle("Settings").click();
  await expect(page.getByText("Theme")).toBeVisible({ timeout: 2000 });
});

test("settings panel closes via backdrop click", async ({ page }) => {
  await page.goto("/");
  await page.getByTitle("Settings").click();
  await expect(page.getByText("Theme")).toBeVisible({ timeout: 2000 });
  await page.locator(".fixed.inset-0.z-40").click();
  await expect(page.getByText("Theme")).not.toBeVisible({ timeout: 2000 });
});

test("settings panel closes via X button", async ({ page }) => {
  await page.goto("/");
  await page.getByTitle("Settings").click();
  await expect(page.getByText("Settings")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "✕" }).click();
  await expect(page.getByText("Theme")).not.toBeVisible({ timeout: 2000 });
});

test("settings shows configured models", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByTitle("Settings").click();
  await expect(page.getByText("claude-opus-4").first()).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("anthropic").first()).toBeVisible();
});

test("settings shows Loading when config not yet received", async ({ page }) => {
  await page.goto("/");
  await page.getByTitle("Settings").click();
  await expect(page.getByText("Loading configuration…")).toBeVisible({ timeout: 2000 });
});

// ── Sidebar busy indicator ─────────────────────────────────────────────────

test("sidebar shows busy pulse for busy session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-busy", Title: "Busy One" })],
  });
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "sb-busy", Busy: true },
  });
  const sessionItem = page.getByText("Busy One").locator("..");
  await expect(sessionItem.locator(".animate-pulse")).toBeVisible({ timeout: 2000 });
});

test("sidebar busy pulse disappears when agent done", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-done", Title: "Done Session" })],
  });
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "sb-done", Busy: true },
  });
  const sessionItem = page.getByText("Done Session").locator("..");
  await expect(sessionItem.locator(".animate-pulse")).toBeVisible({ timeout: 2000 });

  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "sb-done", Busy: false },
  });
  await expect(sessionItem.locator(".animate-pulse")).not.toBeVisible({ timeout: 2000 });
});

// ── Status bar ─────────────────────────────────────────────────────────────

test("status bar is visible with connection status", async ({ page }) => {
  await page.goto("/");
  await expect(
    page.getByText("Connected").or(page.getByText("Disconnected"))
  ).toBeVisible({ timeout: 3000 });
});

test("status bar shows LSP server", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "gopls", state: "running", diagnosticCount: 0 },
  });
  await expect(page.getByText("gopls")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("LSP")).toBeVisible();
});

test("status bar shows LSP diagnostic count when nonzero", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "tsserver", state: "running", diagnosticCount: 3 },
  });
  await expect(page.getByText("(3)")).toBeVisible({ timeout: 2000 });
});

test("status bar shows MCP server", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "filesystem", status: "connected" }] },
  });
  await expect(page.getByText("filesystem")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("MCP")).toBeVisible();
});

// ── Permission dialog ──────────────────────────────────────────────────────

test("permission dialog appears on permission_request", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-1", Title: "Perm Session" })],
  });
  await expect(page.getByText("Perm Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("Perm Session").click();
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "req-1",
      SessionID: "perm-1",
      ToolCallID: "tc-1",
      ToolName: "bash",
      Description: "Run a shell command",
      Action: "execute",
      Path: "/tmp/test.sh",
      Params: {},
    },
  });
  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Run a shell command")).toBeVisible();
  await expect(page.getByRole("button", { name: "Allow", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Allow always" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Deny", exact: true })).toBeVisible();
});

test("permission dialog disappears on permission_notification", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-2", Title: "Perm2" })],
  });
  await expect(page.getByText("Perm2")).toBeVisible({ timeout: 3000 });
  await page.getByText("Perm2").click();
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "req-2",
      SessionID: "perm-2",
      ToolCallID: "tc-2",
      ToolName: "read_file",
      Description: "Read a file",
      Action: "read",
      Path: "/etc/hosts",
      Params: {},
    },
  });
  await expect(page.getByText("read_file")).toBeVisible({ timeout: 2000 });

  await sendMockWSMessage(page, {
    type: "permission_notification",
    payload: { ToolCallID: "tc-2", Granted: true, Denied: false },
  });
  await expect(page.getByText("read_file")).not.toBeVisible({ timeout: 2000 });
});

// ── Reconnection banner ────────────────────────────────────────────────────

test("reconnecting banner appears when disconnected", async ({ page }) => {
  await page.goto("/");
  // Simulate disconnection by sending disconnect event
  await sendMockWSMessage(page, {
    type: "_disconnected",
    payload: null,
  });
  // The banner should appear
  await expect(page.getByText("Reconnecting…")).toBeVisible({ timeout: 2000 });
});
