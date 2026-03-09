/**
 * Model-switch integration tests.
 *
 * Model selection now persists in the database via set_session_models,
 * not via per-message overrides in send_message. These tests verify:
 *  1. Selecting a model sends set_session_models with correct provider/model.
 *  2. The send_message payload does NOT contain largeModel/smallModel overrides.
 *  3. The session_updated event from the server updates the displayed model.
 *  4. Per-session independence is maintained.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Helper ───────────────────────────────────────────────────────────────────

async function getSentMessages(page: import("@playwright/test").Page) {
  return page.evaluate(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: unknown;
    }>;
    return sent.filter((m) => m.type === "send_message").map((m) => m.payload);
  });
}

// ── set_session_models command ────────────────────────────────────────────────

test("selecting large model sends set_session_models with provider and model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-lg", Title: "Large Switch" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Large Switch")).toBeVisible({ timeout: 3000 });
  await page.getByText("Large Switch").click();

  await expect(page.locator("header button[title='Large (strong) model']")).toBeVisible({ timeout: 3000 });
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; largeModel: { provider: string; model: string }; smallModel: unknown };
  expect(p.sessionID).toBe("sw-lg");
  expect(p.largeModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
});

test("selecting small model sends set_session_models with correct small provider/model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-sm", Title: "Small Switch" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
        openai: { models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini", contextWindow: 128000 }] },
      },
    }),
  });
  await expect(page.getByText("Small Switch")).toBeVisible({ timeout: 3000 });
  await page.getByText("Small Switch").click();

  await expect(page.locator("header button[title='Small (fast) model']")).toBeVisible({ timeout: 3000 });
  await page.locator("header button[title='Small (fast) model']").click();
  await page.locator("div.absolute").getByText("gpt-4o-mini").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; smallModel: { provider: string; model: string } };
  expect(p.sessionID).toBe("sw-sm");
  expect(p.smallModel).toEqual({ provider: "openai", model: "gpt-4o-mini" });
});

test("set_session_models includes both models", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-both", Title: "Both Switch" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
        openai: { models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini", contextWindow: 128000 }] },
      },
    }),
  });
  await expect(page.getByText("Both Switch")).toBeVisible({ timeout: 3000 });
  await page.getByText("Both Switch").click();

  // Pick large model
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();
  await waitForWSSend(page, "set_session_models");

  // Pick small model — second set_session_models command
  await page.locator("header button[title='Small (fast) model']").click();
  await page.locator("div.absolute").getByText("gpt-4o-mini").click();

  const secondCmd = await page.waitForFunction(
    () => {
      const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
        type: string;
        payload: Record<string, unknown>;
      }>;
      const cmds = sent.filter((m) => m.type === "set_session_models");
      return cmds.length >= 2 ? cmds[cmds.length - 1] : null;
    },
    { timeout: 5_000 }
  );
  const last = await secondCmd.jsonValue() as { payload: { smallModel: { provider: string; model: string } } };
  expect(last.payload.smallModel).toEqual({ provider: "openai", model: "gpt-4o-mini" });
});

// ── send_message has no overrides ────────────────────────────────────────────

test("send_message does not include largeModel or smallModel overrides", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-no-ov", Title: "No Override" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("No Override")).toBeVisible({ timeout: 3000 });
  await page.getByText("No Override").click();

  // Switch model
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();
  await waitForWSSend(page, "set_session_models");

  // Send a message
  await page.getByPlaceholder("Message… (Enter to send)").fill("hello");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;
  expect(payload.largeModel).toBeUndefined();
  expect(payload.smallModel).toBeUndefined();
  expect(payload.content).toBe("hello");
});

test("send_message never contains model overrides even on default model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-def", Title: "Default Model" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Default Model")).toBeVisible({ timeout: 3000 });
  await page.getByText("Default Model").click();

  await page.getByPlaceholder("Message… (Enter to send)").fill("hello default");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;
  expect(payload.largeModel).toBeUndefined();
  expect(payload.smallModel).toBeUndefined();
  expect(payload.sessionID).toBe("sw-def");
});

// ── session_updated reflects model change in header ──────────────────────────

test("session_updated with model fields updates header model button", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-upd", Title: "Update Model" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Update Model")).toBeVisible({ timeout: 3000 });
  await page.getByText("Update Model").click();

  // Server sends session_updated with model fields filled in
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({
      ID: "sw-upd",
      Title: "Update Model",
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-haiku-4",
    }),
  });

  // Header large model button should now show claude-haiku-4
  await expect(
    page.locator("header button[title='Large (strong) model']").filter({ hasText: "claude-haiku-4" })
  ).toBeVisible({ timeout: 2000 });
});

// ── Per-session independence ──────────────────────────────────────────────────

test("switching model in session A does not affect session B", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "psw-1", Title: "Session A" }),
      makeSession({ ID: "psw-2", Title: "Session B" }),
    ],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });

  // Change Session A model
  await expect(page.getByText("Session A")).toBeVisible({ timeout: 3000 });
  await page.getByText("Session A").click();
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();

  // Session A button shows haiku
  await expect(
    page.locator("header button[title='Large (strong) model']").filter({ hasText: "claude-haiku-4" })
  ).toBeVisible({ timeout: 2000 });

  // Switch to Session B — should show opus (default)
  await page.getByText("Session B").click();
  await expect(
    page.locator("header button[title='Large (strong) model']").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });
});

// ── Model override persists across sends in same session ─────────────────────

test("model override persists for subsequent messages via DB", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-persist", Title: "Persist Session" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Persist Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("Persist Session").click();

  // Switch model once
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();
  await waitForWSSend(page, "set_session_models");

  // Simulate server confirming the session model update
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({
      ID: "sw-persist",
      Title: "Persist Session",
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-haiku-4",
    }),
  });

  // Send two messages — neither should have model overrides in payload
  await page.getByPlaceholder("Message… (Enter to send)").fill("msg one");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitForWSSend(page, "send_message");

  await page.getByPlaceholder("Message… (Enter to send)").fill("msg two");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
    }>;
    return sent.filter((m) => m.type === "send_message").length >= 2;
  }, { timeout: 5000 });

  const msgs = await getSentMessages(page) as Array<Record<string, unknown>>;
  for (const msg of msgs) {
    expect(msg.largeModel).toBeUndefined();
    expect(msg.smallModel).toBeUndefined();
  }
});
