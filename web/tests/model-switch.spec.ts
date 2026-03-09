/**
 * Model-switch integration tests.
 *
 * These tests verify that when a user selects a different model, the
 * subsequent send_message WebSocket payload contains the correct
 * largeModel / smallModel overrides — proving the frontend actually
 * passes the selection through to the backend.
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

// ── Helper ──────────────────────────────────────────────────────────────────

/** Returns all send_message WS payloads sent so far. */
async function getSentMessages(page: import("@playwright/test").Page) {
  return page.evaluate(() => {
    const sent = (window as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: unknown;
    }>;
    return sent.filter((m) => m.type === "send_message").map((m) => m.payload);
  });
}

// ── Default model (no override) ─────────────────────────────────────────────

test("send_message has no model overrides when using default model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-def", Title: "Default Model" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Default Model")).toBeVisible({ timeout: 3000 });
  await page.getByText("Default Model").click();

  // Send a message without changing the model
  await page.getByPlaceholder("Message… (Enter to send)").fill("hello default");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;

  // No model overrides — backend uses its global config
  expect(payload.largeModel).toBeUndefined();
  expect(payload.smallModel).toBeUndefined();
  expect(payload.content).toBe("hello default");
  expect(payload.sessionID).toBe("sw-def");
});

// ── Large model override ─────────────────────────────────────────────────────

test("send_message includes largeModel override after switching large model", async ({ page }) => {
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
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Large Switch")).toBeVisible({ timeout: 3000 });
  await page.getByText("Large Switch").click();

  // Switch large model to claude-haiku-4
  await expect(
    page.locator("header button[title='Large (strong) model']")
  ).toBeVisible({ timeout: 3000 });
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();

  // Send a message
  await page.getByPlaceholder("Message… (Enter to send)").fill("hello haiku");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;

  // largeModel override should reflect the selected model
  expect(payload.content).toBe("hello haiku");
  expect(payload.largeModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
  // smallModel was not changed so no override
  expect(payload.smallModel).toBeUndefined();
});

// ── Small model override ─────────────────────────────────────────────────────

test("send_message includes smallModel override after switching small model", async ({ page }) => {
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
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
        openai: { models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini" }] },
      },
    }),
  });
  await expect(page.getByText("Small Switch")).toBeVisible({ timeout: 3000 });
  await page.getByText("Small Switch").click();

  // Switch small model to gpt-4o-mini
  await expect(
    page.locator("header button[title='Small (fast) model']")
  ).toBeVisible({ timeout: 3000 });
  await page.locator("header button[title='Small (fast) model']").click();
  await page.locator("div.absolute").getByText("gpt-4o-mini").click();

  // Send a message
  await page.getByPlaceholder("Message… (Enter to send)").fill("hello mini");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;

  expect(payload.content).toBe("hello mini");
  // smallModel override should reflect the selected openai model
  expect(payload.smallModel).toEqual({ provider: "openai", model: "gpt-4o-mini" });
  // largeModel was not changed
  expect(payload.largeModel).toBeUndefined();
});

// ── Both overrides ───────────────────────────────────────────────────────────

test("send_message includes both overrides when both models are changed", async ({ page }) => {
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
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
        openai: { models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini" }] },
      },
    }),
  });
  await expect(page.getByText("Both Switch")).toBeVisible({ timeout: 3000 });
  await page.getByText("Both Switch").click();

  // Switch large model to claude-haiku-4
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();

  // Switch small model to gpt-4o-mini
  await page.locator("header button[title='Small (fast) model']").click();
  await page.locator("div.absolute").getByText("gpt-4o-mini").click();

  // Send a message
  await page.getByPlaceholder("Message… (Enter to send)").fill("both models");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;

  expect(payload.content).toBe("both models");
  expect(payload.largeModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
  expect(payload.smallModel).toEqual({ provider: "openai", model: "gpt-4o-mini" });
});

// ── Per-session independence ─────────────────────────────────────────────────

test("model selection is per-session: different sessions send different overrides", async ({ page }) => {
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
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
      },
    }),
  });

  // Session A: change large model to claude-haiku-4
  await expect(page.getByText("Session A")).toBeVisible({ timeout: 3000 });
  await page.getByText("Session A").click();
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();

  // Send from Session A — should have largeModel override
  await page.getByPlaceholder("Message… (Enter to send)").fill("from A");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitForWSSend(page, "send_message");

  const sentAfterA = await getSentMessages(page);
  const msgA = sentAfterA.find(
    (p: unknown) => (p as Record<string, unknown>).content === "from A"
  ) as Record<string, unknown> | undefined;
  expect(msgA?.largeModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });

  // Switch to Session B (no model change) — send a message
  await page.getByText("Session B").click();
  await page.getByPlaceholder("Message… (Enter to send)").fill("from B");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  // Wait for the Session B message
  await page.waitForFunction(() => {
    const sent = (window as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: Record<string, unknown>;
    }>;
    return sent.some((m) => m.type === "send_message" && m.payload?.content === "from B");
  }, { timeout: 5000 });

  const sentAfterB = await getSentMessages(page);
  const msgB = sentAfterB.find(
    (p: unknown) => (p as Record<string, unknown>).content === "from B"
  ) as Record<string, unknown> | undefined;
  // Session B never had a model change — no overrides
  expect(msgB?.largeModel).toBeUndefined();
  expect(msgB?.smallModel).toBeUndefined();
});

// ── Override persists across multiple sends ──────────────────────────────────

test("model override persists for subsequent messages in same session", async ({ page }) => {
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
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Persist Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("Persist Session").click();

  // Switch large model once
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();

  // Send first message
  await page.getByPlaceholder("Message… (Enter to send)").fill("msg one");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitForWSSend(page, "send_message");

  // Send second message without changing model again
  await page.getByPlaceholder("Message… (Enter to send)").fill("msg two");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  await page.waitForFunction(() => {
    const sent = (window as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: Record<string, unknown>;
    }>;
    return sent.filter((m) => m.type === "send_message").length >= 2;
  }, { timeout: 5000 });

  const allSent = await getSentMessages(page);
  const msgs = allSent as Array<Record<string, unknown>>;

  for (const msg of msgs) {
    expect(msg.largeModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
  }
});
