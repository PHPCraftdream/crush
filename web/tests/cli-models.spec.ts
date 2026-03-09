/**
 * CLI model end-to-end UI tests.
 *
 * Covers for every CLI model (claude, gemini, qwen, codex):
 *   1. Model appears in the provider config and can be selected from the dropdown.
 *   2. Selecting a model sends set_session_models with the correct provider/model.
 *   3. An assistant response is displayed in the chat after send_message.
 *   4. A permission_request from a tool call shows the permission dialog.
 *   5. Clicking "Allow" sends grant_permission and dismisses the dialog.
 *   6. Clicking "Allow always" sends grant_permission_persistent and dismisses the dialog.
 *   7. After "Allow always" a second tool call is auto-approved (no dialog shown).
 *   8. Clicking "Deny" sends deny_permission and dismisses the dialog.
 *
 * NOTE: The app auto-selects the first session when `sessions_list` is received,
 * so tests do NOT need to click on the session title — the session is already active.
 */

import { test, expect, Page } from "@playwright/test";
import {
  setupMockWS,
  sendMockWSMessage,
  waitForWSSend,
} from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

// ── Model catalogue ───────────────────────────────────────────────────────────

const CLI_MODELS = [
  { id: "cli-claude-sonnet",          name: "Claude Sonnet (CLI)",                provider: "local-cli" },
  { id: "cli-claude-opus",            name: "Claude Opus (CLI)",                  provider: "local-cli" },
  { id: "cli-claude-sonnet-thinking", name: "Claude Sonnet Thinking (CLI)",       provider: "local-cli" },
  { id: "cli-claude-opus-thinking",   name: "Claude Opus Thinking (CLI)",         provider: "local-cli" },
  { id: "cli-gemini-flash",           name: "Gemini 3 Flash (CLI)",               provider: "local-cli" },
  { id: "cli-gemini-pro",             name: "Gemini 3.1 Pro (CLI)",               provider: "local-cli" },
  { id: "cli-qwen",                   name: "Qwen 3.5 Plus (CLI)",                provider: "local-cli" },
  { id: "cli-codex",                  name: "Codex (gpt-5.3-codex, CLI)",         provider: "local-cli" },
  { id: "cli-codex-gpt-5-4",         name: "Codex (gpt-5.4, CLI)",               provider: "local-cli" },
  { id: "cli-codex-gpt-5-2",         name: "Codex (gpt-5.2-codex, CLI)",         provider: "local-cli" },
  { id: "cli-codex-max",              name: "Codex Max (gpt-5.1-codex-max, CLI)", provider: "local-cli" },
  { id: "cli-codex-gpt-5-2-base",    name: "Codex (gpt-5.2, CLI)",               provider: "local-cli" },
  { id: "cli-codex-mini",            name: "Codex Mini (gpt-5.1-codex-mini, CLI)", provider: "local-cli" },
] as const;

type CliModel = (typeof CLI_MODELS)[number];

// ── Config factory ────────────────────────────────────────────────────────────

function makeCliConfig(models: readonly CliModel[] = CLI_MODELS) {
  return makeConfig({
    models: {
      large: { Provider: "local-cli", Model: models[0].id },
      small: { Provider: "local-cli", Model: models[0].id },
    },
    providers: {
      "local-cli": {
        name: "CLI",
        // enabled: true makes models selectable; type: "cli" marks as CLI provider
        enabled: true,
        type: "cli",
        models: models.map((m) => ({
          id: m.id,
          name: m.name,
          contextWindow: 200_000,
        })),
      },
    },
  });
}

// ── Helpers ───────────────────────────────────────────────────────────────────

/**
 * Navigate to the app, inject sessions + config, and wait for the chat UI to be ready.
 *
 * The app auto-selects the first session when sessions_list arrives, so there
 * is no need to click on the session title in the sidebar.
 */
async function setup(page: Page, sessionID: string, title: string) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: title })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeCliConfig() });
  // Wait until the chat input is enabled (session active + config received)
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 5000 });
}

/** Select a model from the "Large" model dropdown in the header. */
async function selectLargeModel(page: Page, modelName: string) {
  await page.locator("header button[title='Large (strong) model']").click();
  await page.getByPlaceholder("Search models…").fill(modelName);
  await page.locator("div.z-50").getByRole("button", { name: modelName, exact: true }).click();
}

/** Select a model from the "Small" model dropdown in the header. */
async function selectSmallModel(page: Page, modelName: string) {
  await page.locator("header button[title='Small (fast) model']").click();
  await page.getByPlaceholder("Search models…").fill(modelName);
  await page.locator("div.z-50").getByRole("button", { name: modelName, exact: true }).click();
}

function makePermission(overrides: Record<string, unknown> = {}) {
  return {
    ID: "perm-1",
    SessionID: "sess-cli",
    ToolCallID: "tc-1",
    ToolName: "Bash",
    Description: "Run a shell command",
    Action: "execute",
    Path: "/tmp",
    Params: {},
    ...overrides,
  };
}

// ── Per-model: selection + response ──────────────────────────────────────────

for (const model of CLI_MODELS) {
  test.describe(`CLI model: ${model.name}`, () => {
    test.beforeEach(async ({ page }) => {
      await setupMockWS(page);
      await page.route("/auth/check", (route) =>
        route.fulfill({ status: 200, body: "OK" })
      );
    });

    test(`${model.id}: appears in config and can be selected as large model`, async ({ page }) => {
      const sessionID = `sel-large-${model.id}`;
      await setup(page, sessionID, `Session ${sessionID}`);

      await selectLargeModel(page, model.name);

      const cmd = await waitForWSSend(page, "set_session_models");
      const payload = cmd.payload as {
        largeModel: { provider: string; model: string };
      };
      expect(payload.largeModel).toEqual({
        provider: model.provider,
        model: model.id,
      });
    });

    test(`${model.id}: can be selected as small model`, async ({ page }) => {
      const sessionID = `sel-small-${model.id}`;
      await setup(page, sessionID, `Session ${sessionID}`);

      await selectSmallModel(page, model.name);

      const cmd = await waitForWSSend(page, "set_session_models");
      const payload = cmd.payload as {
        smallModel: { provider: string; model: string };
      };
      expect(payload.smallModel).toEqual({
        provider: model.provider,
        model: model.id,
      });
    });

    test(`${model.id}: assistant response is displayed in chat`, async ({ page }) => {
      const sessionID = `chat-${model.id}`;
      await setup(page, sessionID, `Session ${sessionID}`);

      // Select this model
      await selectLargeModel(page, model.name);
      await waitForWSSend(page, "set_session_models");

      // Confirm model switch via session_updated
      await sendMockWSMessage(page, {
        type: "session_updated",
        payload: makeSession({
          ID: sessionID,
          LargeModelProvider: model.provider,
          LargeModelID: model.id,
        }),
      });

      // Send a message
      await page.getByPlaceholder("Message… (Enter to send)").fill("Hello from test");
      await page.getByRole("button", { name: "Send", exact: true }).click();

      const sent = await waitForWSSend(page, "send_message");
      expect((sent.payload as Record<string, unknown>).content).toBe("Hello from test");

      // Backend sends response
      const responseText = `Response from ${model.name}`;
      await sendMockWSMessage(page, {
        type: "message_created",
        payload: makeMessage({
          ID: `resp-${model.id}`,
          SessionID: sessionID,
          Role: "assistant",
          Parts: [{ type: "text", Text: responseText }],
          Model: model.id,
          Provider: model.provider,
        }),
      });

      await expect(page.getByText(responseText)).toBeVisible({ timeout: 3000 });
    });
  });
}

// ── Permission flows ──────────────────────────────────────────────────────────

test.describe("CLI model permissions — Allow", () => {
  test.beforeEach(async ({ page }) => {
    await setupMockWS(page);
    await page.route("/auth/check", (route) =>
      route.fulfill({ status: 200, body: "OK" })
    );
  });

  test("permission dialog appears when tool call requires approval", async ({ page }) => {
    await setup(page, "perm-allow-1", "Session perm-allow-1");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-allow-1", ToolName: "Bash", ToolCallID: "tc-allow-1" }),
    });

    await expect(page.getByText("Bash")).toBeVisible({ timeout: 2000 });
    await expect(page.getByRole("button", { name: "Allow", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Allow always" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Deny", exact: true })).toBeVisible();
  });

  test("clicking Allow sends grant_permission with correct permissionID", async ({ page }) => {
    await setup(page, "perm-allow-2", "Session perm-allow-2");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-allow-2", ToolCallID: "tc-allow-2" }),
    });

    await page.getByRole("button", { name: "Allow", exact: true }).click();
    const cmd = await waitForWSSend(page, "grant_permission");
    expect((cmd.payload as { permissionID: string }).permissionID).toBe("p-allow-2");
  });

  test("clicking Allow dismisses the permission dialog", async ({ page }) => {
    await setup(page, "perm-allow-3", "Session perm-allow-3");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-allow-3", ToolName: "Read", ToolCallID: "tc-allow-3" }),
    });

    await expect(page.getByText("Read")).toBeVisible({ timeout: 2000 });
    await page.getByRole("button", { name: "Allow", exact: true }).click();
    await expect(page.getByText("Read")).not.toBeVisible({ timeout: 2000 });
  });

  test("after Allow the chat continues: response appears after permission_notification", async ({ page }) => {
    await setup(page, "perm-allow-4", "Session perm-allow-4");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-allow-4", ToolCallID: "tc-allow-4" }),
    });
    await page.getByRole("button", { name: "Allow", exact: true }).click();
    await waitForWSSend(page, "grant_permission");

    // Backend confirms grant
    await sendMockWSMessage(page, {
      type: "permission_notification",
      payload: { ToolCallID: "tc-allow-4", Granted: true, Denied: false },
    });

    // Backend sends assistant reply
    await sendMockWSMessage(page, {
      type: "message_created",
      payload: makeMessage({
        ID: "msg-after-allow",
        SessionID: "perm-allow-4",
        Role: "assistant",
        Parts: [{ type: "text", Text: "Done, files listed." }],
      }),
    });

    await expect(page.getByText("Done, files listed.")).toBeVisible({ timeout: 3000 });
  });
});

// ── Allow always ──────────────────────────────────────────────────────────────

test.describe("CLI model permissions — Allow always", () => {
  test.beforeEach(async ({ page }) => {
    await setupMockWS(page);
    await page.route("/auth/check", (route) =>
      route.fulfill({ status: 200, body: "OK" })
    );
  });

  test("clicking Allow always sends grant_permission_persistent", async ({ page }) => {
    await setup(page, "perm-always-1", "Session perm-always-1");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-always-1", ToolCallID: "tc-always-1" }),
    });

    await page.getByRole("button", { name: "Allow always" }).click();
    const cmd = await waitForWSSend(page, "grant_permission_persistent");
    expect((cmd.payload as { permissionID: string }).permissionID).toBe("p-always-1");
  });

  test("clicking Allow always sends ONLY grant_permission_persistent (not grant_permission)", async ({ page }) => {
    await setup(page, "perm-always-2", "Session perm-always-2");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-always-2", ToolCallID: "tc-always-2" }),
    });

    await page.getByRole("button", { name: "Allow always" }).click();
    await waitForWSSend(page, "grant_permission_persistent");

    const sentNonPersistent = await page.evaluate(() => {
      const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
      return sent.some((m) => m.type === "grant_permission");
    });
    expect(sentNonPersistent).toBe(false);
  });

  test("clicking Allow always dismisses the permission dialog", async ({ page }) => {
    await setup(page, "perm-always-3", "Session perm-always-3");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-always-3", ToolName: "Write", ToolCallID: "tc-always-3" }),
    });

    await expect(page.getByText("Write")).toBeVisible({ timeout: 2000 });
    await page.getByRole("button", { name: "Allow always" }).click();
    await expect(page.getByText("Write")).not.toBeVisible({ timeout: 2000 });
  });

  test("after Allow always, second tool call is auto-approved — no dialog shown", async ({ page }) => {
    await setup(page, "perm-always-4", "Session perm-always-4");

    // First request: user clicks "Allow always"
    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-always-4a", ToolName: "Bash", ToolCallID: "tc-always-4a" }),
    });
    await page.getByRole("button", { name: "Allow always" }).click();
    await waitForWSSend(page, "grant_permission_persistent");

    // Backend auto-approves second request — sends notification without permission_request
    await sendMockWSMessage(page, {
      type: "permission_notification",
      payload: { ToolCallID: "tc-always-4b", Granted: true, Denied: false },
    });

    // No permission dialog should be visible
    await expect(page.getByRole("button", { name: "Allow", exact: true })).not.toBeVisible({ timeout: 1000 });
    await expect(page.getByRole("button", { name: "Allow always" })).not.toBeVisible();

    // Response continues without interruption
    await sendMockWSMessage(page, {
      type: "message_created",
      payload: makeMessage({
        ID: "msg-always-4",
        SessionID: "perm-always-4",
        Role: "assistant",
        Parts: [{ type: "text", Text: "Auto-approved and completed." }],
      }),
    });
    await expect(page.getByText("Auto-approved and completed.")).toBeVisible({ timeout: 3000 });
  });

  test("after Allow always, multiple subsequent tool calls are auto-approved (session-wide)", async ({ page }) => {
    await setup(page, "perm-always-5", "Session perm-always-5");

    // Grant persistent for first request
    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-always-5a", ToolName: "Bash", ToolCallID: "tc-5a" }),
    });
    await page.getByRole("button", { name: "Allow always" }).click();
    await waitForWSSend(page, "grant_permission_persistent");

    // Second and third requests: auto-approved by backend
    await sendMockWSMessage(page, {
      type: "permission_notification",
      payload: { ToolCallID: "tc-5b", Granted: true, Denied: false },
    });
    await sendMockWSMessage(page, {
      type: "permission_notification",
      payload: { ToolCallID: "tc-5c", Granted: true, Denied: false },
    });

    // Dialog must not appear for 2nd or 3rd requests
    await expect(page.getByRole("button", { name: "Allow", exact: true })).not.toBeVisible({ timeout: 1000 });

    await sendMockWSMessage(page, {
      type: "message_created",
      payload: makeMessage({
        ID: "msg-always-5",
        SessionID: "perm-always-5",
        Role: "assistant",
        Parts: [{ type: "text", Text: "All three done." }],
      }),
    });
    await expect(page.getByText("All three done.")).toBeVisible({ timeout: 3000 });
  });
});

// ── Deny ──────────────────────────────────────────────────────────────────────

test.describe("CLI model permissions — Deny", () => {
  test.beforeEach(async ({ page }) => {
    await setupMockWS(page);
    await page.route("/auth/check", (route) =>
      route.fulfill({ status: 200, body: "OK" })
    );
  });

  test("clicking Deny sends deny_permission with correct permissionID", async ({ page }) => {
    await setup(page, "perm-deny-1", "Session perm-deny-1");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-deny-1", ToolCallID: "tc-deny-1" }),
    });

    await page.getByRole("button", { name: "Deny", exact: true }).click();
    const cmd = await waitForWSSend(page, "deny_permission");
    expect((cmd.payload as { permissionID: string }).permissionID).toBe("p-deny-1");
  });

  test("clicking Deny dismisses the permission dialog", async ({ page }) => {
    await setup(page, "perm-deny-2", "Session perm-deny-2");

    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-deny-2", ToolName: "Glob", ToolCallID: "tc-deny-2" }),
    });

    await expect(page.getByText("Glob")).toBeVisible({ timeout: 2000 });
    await page.getByRole("button", { name: "Deny", exact: true }).click();
    await expect(page.getByText("Glob")).not.toBeVisible({ timeout: 2000 });
  });

  test("after Deny, next request for same tool still shows dialog (deny does not persist)", async ({ page }) => {
    await setup(page, "perm-deny-3", "Session perm-deny-3");

    // First request: denied
    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-deny-3a", ToolName: "Bash", ToolCallID: "tc-deny-3a" }),
    });
    await page.getByRole("button", { name: "Deny", exact: true }).click();
    await waitForWSSend(page, "deny_permission");

    // Second request for same tool — dialog must appear again
    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "p-deny-3b", ToolName: "Bash", ToolCallID: "tc-deny-3b" }),
    });

    await expect(page.getByText("Bash")).toBeVisible({ timeout: 2000 });
    await expect(page.getByRole("button", { name: "Allow", exact: true })).toBeVisible();
  });
});

// ── Mixed flow ────────────────────────────────────────────────────────────────

test.describe("CLI model permissions — mixed flow", () => {
  test.beforeEach(async ({ page }) => {
    await setupMockWS(page);
    await page.route("/auth/check", (route) =>
      route.fulfill({ status: 200, body: "OK" })
    );
  });

  test("Allow once then Allow always: second dialog appears, third is auto-approved", async ({ page }) => {
    await setup(page, "perm-mixed-1", "Session perm-mixed-1");

    // First request: Allow once
    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "pm-1a", ToolName: "Bash", ToolCallID: "tc-m1a" }),
    });
    await page.getByRole("button", { name: "Allow", exact: true }).click();
    await waitForWSSend(page, "grant_permission");

    // Second request: dialog appears again (Allow once doesn't persist)
    await sendMockWSMessage(page, {
      type: "permission_request",
      payload: makePermission({ ID: "pm-1b", ToolName: "Bash", ToolCallID: "tc-m1b" }),
    });
    await expect(page.getByRole("button", { name: "Allow always" })).toBeVisible({ timeout: 2000 });

    // Click "Allow always" for the second request
    await page.getByRole("button", { name: "Allow always" }).click();
    const persistCmd = await waitForWSSend(page, "grant_permission_persistent");
    expect((persistCmd.payload as { permissionID: string }).permissionID).toBe("pm-1b");

    // Third request: backend auto-approves (no dialog)
    await sendMockWSMessage(page, {
      type: "permission_notification",
      payload: { ToolCallID: "tc-m1c", Granted: true, Denied: false },
    });
    await expect(page.getByRole("button", { name: "Allow", exact: true })).not.toBeVisible({ timeout: 1000 });
  });
});
