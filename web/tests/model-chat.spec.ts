/**
 * Model-switch + chat integration tests.
 *
 * Uses two "real" (in terms of mock config) providers:
 *   - cliprovider / claude-cli   (maps to cli-claude in config)
 *   - glm / glm-5               (maps to glm-5 in config)
 *
 * Tests verify:
 *  1. The correct model override is sent in the send_message payload.
 *  2. The assistant's response appears in the chat after the model is switched.
 *  3. Error events from the backend are surfaced as a visible banner.
 */

import { test, expect } from "@playwright/test";
import {
  setupMockWS,
  sendMockWSMessage,
  waitForWSSend,
} from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

/** Config that mirrors the user's two working models. */
function makeTwoModelConfig() {
  return makeConfig({
    models: {
      large: { Provider: "cliprovider", Model: "claude-cli" },
      small: { Provider: "glm", Model: "glm-5" },
    },
    providers: {
      cliprovider: {
        models: [{ id: "claude-cli", name: "claude-cli" }],
      },
      glm: {
        models: [{ id: "glm-5", name: "glm-5" }],
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

// ── Helpers ──────────────────────────────────────────────────────────────────

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
    const sent = (window as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: unknown;
    }>;
    return sent
      .filter((m) => m.type === "send_message")
      .map((m) => m.payload) as Array<Record<string, unknown>>;
  });
  return payloads[payloads.length - 1];
}

// ── Default model — no override sent ────────────────────────────────────────

test("default model: send_message has no override, response appears in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-def", "Default Chat");

  // Send a message without changing the model
  await page.getByPlaceholder("Message… (Enter to send)").fill("hello");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;
  expect(payload.content).toBe("hello");
  expect(payload.largeModel).toBeUndefined();

  // Simulate assistant response
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

// ── Switch to cli-claude, chat, verify override + response visible ────────────

test("cli-claude model: override sent and response appears in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-cli", "CLI Claude Chat");

  // Select cli-claude from the large model dropdown
  await expect(
    page.locator("header button[title='Large (strong) model']")
  ).toBeVisible({ timeout: 3000 });
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.absolute").getByText("claude-cli").click();

  // Verify header shows the selected model
  await expect(
    page.locator("header button[title='Large (strong) model']").filter({ hasText: "claude-cli" })
  ).toBeVisible({ timeout: 2000 });

  // Send a message
  await page.getByPlaceholder("Message… (Enter to send)").fill("what model are you?");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  // Verify WS payload contains the model override
  await waitForWSSend(page, "send_message");
  const payload = await getLastSentMessage(page);
  expect(payload.content).toBe("what model are you?");
  expect(payload.largeModel).toEqual({ provider: "cliprovider", model: "claude-cli" });

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

// ── Switch to glm-5, chat, verify override + response visible ────────────────

test("glm-5 small model: override sent and response appears in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-glm", "GLM Chat");

  // Select glm-5 from the small model dropdown
  await page.locator("header button[title='Small (fast) model']").click();
  await page.locator("div.absolute").getByText("glm-5").click();

  // Verify header shows the selected model
  await expect(
    page.locator("header button[title='Small (fast) model']").filter({ hasText: "glm-5" })
  ).toBeVisible({ timeout: 2000 });

  // Send a message
  await page.getByPlaceholder("Message… (Enter to send)").fill("respond fast");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  await waitForWSSend(page, "send_message");
  const payload = await getLastSentMessage(page);
  expect(payload.content).toBe("respond fast");
  expect(payload.smallModel).toEqual({ provider: "glm", model: "glm-5" });

  // Simulate response
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

// ── Switch between models across messages in same session ───────────────────

test("switching model mid-session: each message uses the current model", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-switch", "Mid-Session Switch");

  // First message — default model
  await page.getByPlaceholder("Message… (Enter to send)").fill("first message");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitForWSSend(page, "send_message");
  const first = await getLastSentMessage(page);
  expect(first.largeModel).toBeUndefined();

  // Simulate assistant reply
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

  // Second message — glm-5 override
  await page.getByPlaceholder("Message… (Enter to send)").fill("second message");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  await page.waitForFunction(() => {
    const sent = (window as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: Record<string, unknown>;
    }>;
    return sent.filter((m) => m.type === "send_message").length >= 2;
  }, { timeout: 5000 });

  const second = await getLastSentMessage(page);
  expect(second.content).toBe("second message");
  expect(second.largeModel).toEqual({ provider: "glm", model: "glm-5" });

  // Simulate assistant reply
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

// ── Error banner appears when backend returns an error ───────────────────────

test("agent error event shows error banner in chat", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-err", "Error Test");

  // Send a message
  await page.getByPlaceholder("Message… (Enter to send)").fill("trigger error");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitForWSSend(page, "send_message");

  // Simulate an error from the backend (e.g. model API failure)
  await sendMockWSMessage(page, {
    type: "error",
    id: "err-1",
    payload: null,
  });

  // The error banner should appear (the WS message uses .error field for text)
  // Since the mock sends no error string, it shows "Unknown error"
  await expect(page.getByText(/Unknown error|⚠/)).toBeVisible({ timeout: 3000 });
});

test("error banner can be dismissed", async ({ page }) => {
  await setupSessionAndConfig(page, "mc-err2", "Dismiss Test");

  // Trigger the error banner via WS error event
  await sendMockWSMessage(page, { type: "error" });
  await expect(page.getByText(/Unknown error/)).toBeVisible({ timeout: 3000 });

  // Click the ✕ dismiss button
  await page.getByRole("button", { name: "✕" }).last().click();
  await expect(page.getByText(/Unknown error/)).not.toBeVisible({ timeout: 2000 });
});
