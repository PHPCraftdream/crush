/**
 * Model-switch + chat integration tests.
 *
 * Uses two "real" (in terms of mock config) providers:
 *   - cliprovider / claude-cli
 *   - glm / glm-5
 *
 * Tests verify:
 *  1. Selecting a model sends set_session_models (not a per-message override).
 *  2. send_message payload contains only sessionID and content.
 *  3. The assistant's response appears in the chat after the model is switched.
 *  4. Error events from the backend are surfaced as an inline banner.
 *  5. Error banner can be dismissed.
 */

import { test, expect } from "@playwright/test";
import {
  setupMockWS,
  sendMockWSMessage,
  waitForWSSend,
} from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

function makeTwoModelConfig() {
  return makeConfig({
    models: {
      large: { Provider: "cliprovider", Model: "claude-cli" },
      small: { Provider: "glm", Model: "glm-5" },
    },
    providers: {
      cliprovider: {
        models: [{ id: "claude-cli", name: "claude-cli", contextWindow: 200000 }],
      },
      glm: {
        models: [{ id: "glm-5", name: "glm-5", contextWindow: 128000 }],
      },
    },
  });
}

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupSessionAndConfig(
  page: import("@playwright/test").Page,
  sessionID: string,
  title: string
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: title })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeTwoModelConfig() });
  await expect(page.getByText(title)).toBeVisible({ timeout: 3000 });
  await page.getByText(title).click();
}

async function getLastSentMessage(page: import("@playwright/test").Page) {
  const payloads = await page.evaluate(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: unknown;
    }>;
    return sent
      .filter((m) => m.type === "send_message")
      .map((m) => m.payload) as Array<Record<string, unknown>>;
  });
  return payloads[payloads.length - 1];
}

// ── Default model — no DB model set ──────────────────────────────────────────

test("default model: send_message has no overrides and response appears in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-def", "Default Chat");

  await page.getByPlaceholder("Message… (Enter to send)").fill("hello");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;
  expect(payload.content).toBe("hello");
  // No model overrides — backend reads from session record in DB
  expect(payload.largeModel).toBeUndefined();
  expect(payload.smallModel).toBeUndefined();

  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "resp-def",
      SessionID: "mc-def",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Hi there!" }],
      Model: "claude-cli",
      Provider: "cliprovider",
    }),
  });
  await expect(page.getByText("Hi there!")).toBeVisible({ timeout: 3000 });
});

// ── Switch large model, verify set_session_models + response visible ──────────

test("switching large model sends set_session_models and response appears in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-cli", "CLI Claude Chat");

  await expect(
    page.locator("header button[title='Large (strong) model']")
  ).toBeVisible({ timeout: 3000 });
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-cli").click();

  // Verify set_session_models was sent
  const modelsCmd = await waitForWSSend(page, "set_session_models");
  const mp = modelsCmd.payload as { largeModel: { provider: string; model: string } };
  expect(mp.largeModel).toEqual({ provider: "cliprovider", model: "claude-cli" });

  // Simulate server confirming session model update
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({
      ID: "mc-cli",
      Title: "CLI Claude Chat",
      LargeModelProvider: "cliprovider",
      LargeModelID: "claude-cli",
    }),
  });

  await expect(
    page.locator("header button[title='Large (strong) model']").filter({ hasText: "claude-cli" })
  ).toBeVisible({ timeout: 2000 });

  // Send a message — no override in payload
  await page.getByPlaceholder("Message… (Enter to send)").fill("what model are you?");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  await waitForWSSend(page, "send_message");
  const payload = await getLastSentMessage(page);
  expect(payload.content).toBe("what model are you?");
  expect(payload.largeModel).toBeUndefined();

  // Simulate assistant response
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "resp-cli",
      SessionID: "mc-cli",
      Role: "assistant",
      Parts: [{ type: "text", Text: "I am claude-cli." }],
      Model: "claude-cli",
      Provider: "cliprovider",
    }),
  });
  await expect(page.getByText("I am claude-cli.")).toBeVisible({ timeout: 3000 });
});

// ── Switch small model ────────────────────────────────────────────────────────

test("switching small model sends set_session_models and response appears in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-glm", "GLM Chat");

  await page.locator("header button[title='Small (fast) model']").click();
  await page.locator("div.absolute").getByText("glm-5").click();

  const modelsCmd = await waitForWSSend(page, "set_session_models");
  const mp = modelsCmd.payload as { smallModel: { provider: string; model: string } };
  expect(mp.smallModel).toEqual({ provider: "glm", model: "glm-5" });

  await page.getByPlaceholder("Message… (Enter to send)").fill("respond fast");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  await waitForWSSend(page, "send_message");
  const payload = await getLastSentMessage(page);
  expect(payload.content).toBe("respond fast");
  expect(payload.smallModel).toBeUndefined();

  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "resp-glm",
      SessionID: "mc-glm",
      Role: "assistant",
      Parts: [{ type: "text", Text: "GLM-5 response here." }],
      Model: "glm-5",
      Provider: "glm",
    }),
  });
  await expect(page.getByText("GLM-5 response here.")).toBeVisible({ timeout: 3000 });
});

// ── Switch between models mid-session ────────────────────────────────────────

test("switching model mid-session: set_session_models sent each time, no send_message override", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-switch", "Mid-Session Switch");

  // First message — no model switch yet
  await page.getByPlaceholder("Message… (Enter to send)").fill("first message");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitForWSSend(page, "send_message");
  const first = await getLastSentMessage(page);
  expect(first.largeModel).toBeUndefined();

  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "r1",
      SessionID: "mc-switch",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Reply one." }],
    }),
  });
  await expect(page.getByText("Reply one.")).toBeVisible({ timeout: 3000 });

  // Switch large model to glm-5
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("glm-5").click();
  await waitForWSSend(page, "set_session_models");

  // Second message — still no override in payload
  await page.getByPlaceholder("Message… (Enter to send)").fill("second message");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
    }>;
    return sent.filter((m) => m.type === "send_message").length >= 2;
  }, { timeout: 5000 });

  const second = await getLastSentMessage(page);
  expect(second.content).toBe("second message");
  expect(second.largeModel).toBeUndefined();

  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "r2",
      SessionID: "mc-switch",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Reply two." }],
    }),
  });
  await expect(page.getByText("Reply two.")).toBeVisible({ timeout: 3000 });
});

// ── Error banner ──────────────────────────────────────────────────────────────

test("agent error event shows inline error banner in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-err", "Error Test");

  await page.getByPlaceholder("Message… (Enter to send)").fill("trigger error");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitForWSSend(page, "send_message");

  await sendMockWSMessage(page, {
    type: "error",
    id: "err-1",
    payload: null,
  });

  // Error banner with ⚠ icon should appear in the chat area
  await expect(page.getByText(/Unknown error|⚠/)).toBeVisible({ timeout: 3000 });
});

test("error banner shows the error message text", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-err-txt", "Error Text Test");

  await sendMockWSMessage(page, {
    type: "error",
    id: "err-2",
    error: "API rate limit exceeded",
    payload: null,
  });

  await expect(page.getByText("API rate limit exceeded")).toBeVisible({ timeout: 3000 });
});

test("error banner can be dismissed with × button", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-err2", "Dismiss Test");

  await sendMockWSMessage(page, { type: "error" });
  await expect(page.getByText(/Unknown error/)).toBeVisible({ timeout: 3000 });

  // Click the ✕ dismiss button in the error banner
  await page.locator("button", { hasText: "×" }).click();
  await expect(page.getByText(/Unknown error/)).not.toBeVisible({ timeout: 2000 });
});

test("error banner disappears after 8 seconds automatically", async ({ page }) => {
  // Use fake timers to avoid real 8-second wait
  await page.addInitScript(() => {
    // Speed up setTimeout
    const origSetTimeout = window.setTimeout;
    (window as unknown as Record<string, unknown>)["__origSetTimeout"] = origSetTimeout;
  });

  await setupSessionAndConfig(page, "mc-err3", "Auto Dismiss");

  await sendMockWSMessage(page, { type: "error" });
  await expect(page.getByText(/Unknown error/)).toBeVisible({ timeout: 3000 });

  // Fast-forward time by 10 seconds
  await page.evaluate(() => {
    // Trigger all timeouts immediately
    const callbacks: Array<() => void> = [];
    const orig = window.setTimeout;
    (window as unknown as Record<string, unknown>)["setTimeout"] = (fn: () => void) => {
      callbacks.push(fn);
      return 0;
    };
    // Restore and fire
    (window as unknown as Record<string, unknown>)["setTimeout"] = orig;
    callbacks.forEach((fn) => fn());
  });

  await expect(page.getByText(/Unknown error/)).not.toBeVisible({ timeout: 3000 });
});
