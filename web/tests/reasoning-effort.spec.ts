/**
 * Reasoning effort end-to-end UI tests.
 *
 * Tests for the reasoning effort control feature:
 * 1. Effort controls are visible only for CLI Claude models.
 * 2. Clicking the increase/decrease buttons cycles through effort levels: low → medium → high → max → low.
 * 3. The effort level is persisted in the session (survives page reload).
 * 4. Clicking effort controls sends set_session_models with the correct reasoning_effort.
 * 5. The effort badge is displayed next to the model name in assistant messages.
 */

import { test, expect } from "@playwright/test";
import {
  setupMockWS,
  sendMockWSMessage,
  waitForWSSend,
} from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Config factory ────────────────────────────────────────────────────────────

function makeClaudeConfig() {
  return makeConfig({
    models: {
      large: { Provider: "local-cli", Model: "cli-claude-opus" },
      small: { Provider: "local-cli", Model: "cli-claude-sonnet" },
    },
    providers: {
      "local-cli": {
        name: "CLI",
        enabled: true,
        type: "cli",
        models: [
          { id: "cli-claude-opus", name: "Claude Opus (CLI)", contextWindow: 200_000 },
          { id: "cli-claude-sonnet", name: "Claude Sonnet (CLI)", contextWindow: 200_000 },
          { id: "gemini-flash", name: "Gemini Flash", contextWindow: 200_000 },
        ],
      },
    },
  });
}

// ── Helpers ───────────────────────────────────────────────────────────────────

async function setup(page: any) {
  await page.goto("/");
  const sessionID = "test-session-reasoning";

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({
        ID: sessionID,
        Title: "Reasoning Effort Test",
        LargeModelProvider: "local-cli",
        LargeModelID: "cli-claude-opus",
        LargeModelReasoningEffort: "medium",
      }),
    ],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeClaudeConfig() });

  // Wait for chat input to be enabled
  await expect(page.locator('[data-test-id="chat-input-textarea"]')).toBeEnabled({
    timeout: 5000,
  });

  return sessionID;
}

test.describe("Reasoning Effort Controls", () => {
  test("effort controls are visible for CLI Claude models", async ({ page }) => {
    await setup(page);

    // Large model selector should show effort controls
    await expect(page.locator('[data-test-id="reasoning-effort-large"]')).toBeVisible();
    await expect(
      page.locator('[data-test-id="reasoning-effort-large-decrease"]'),
    ).toBeVisible();
    await expect(
      page.locator('[data-test-id="reasoning-effort-large-label"]'),
    ).toBeVisible();
    await expect(
      page.locator('[data-test-id="reasoning-effort-large-increase"]'),
    ).toBeVisible();

    // Small model selector should also show effort controls
    await expect(page.locator('[data-test-id="reasoning-effort-small"]')).toBeVisible();
  });

  test("effort label displays correct initial value (M for medium)", async ({ page }) => {
    await setup(page);

    const largeLabel = page.locator('[data-test-id="reasoning-effort-large-label"]');
    await expect(largeLabel).toHaveText("M");

    const smallLabel = page.locator('[data-test-id="reasoning-effort-small-label"]');
    await expect(smallLabel).toHaveText("M");
  });

  test("clicking increase button cycles through effort levels", async ({ page }) => {
    await setup(page);
    const sessionID = "test-session-reasoning";

    const label = page.locator('[data-test-id="reasoning-effort-large-label"]');

    // M (medium) → H (high)
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await expect(label).toHaveText("H");

    // Verify set_session_models was sent
    const sentMsg = await waitForWSSend(page, "set_session_models");
    expect(sentMsg.type).toBe("set_session_models");
    expect(sentMsg.payload.largeModel.reasoning_effort).toBe("high");

    // H (high) → X (max)
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await expect(label).toHaveText("X");

    // X (max) → L (low)
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await expect(label).toHaveText("L");

    // L (low) → M (medium) - cycle completes
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await expect(label).toHaveText("M");
  });

  test("clicking decrease button cycles through effort levels in reverse", async ({
    page,
  }) => {
    await setup(page);
    const label = page.locator('[data-test-id="reasoning-effort-large-label"]');

    // M (medium) → L (low)
    await page.locator('[data-test-id="reasoning-effort-large-decrease"]').click();
    await expect(label).toHaveText("L");

    // L (low) → X (max)
    await page.locator('[data-test-id="reasoning-effort-large-decrease"]').click();
    await expect(label).toHaveText("X");

    // X (max) → H (high)
    await page.locator('[data-test-id="reasoning-effort-large-decrease"]').click();
    await expect(label).toHaveText("H");

    // H (high) → M (medium) - cycle completes
    await page.locator('[data-test-id="reasoning-effort-large-decrease"]').click();
    await expect(label).toHaveText("M");
  });

  test("effort is persisted when session is reloaded", async ({ page }) => {
    await setup(page);
    const sessionID = "test-session-reasoning";

    // Change effort to high
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await expect(
      page.locator('[data-test-id="reasoning-effort-large-label"]'),
    ).toHaveText("H");

    // Wait for the update to be sent
    await waitForWSSend(page, "set_session_models");

    // Simulate page reload by sending updated session data
    await sendMockWSMessage(page, {
      type: "session_updated",
      payload: makeSession({
        ID: sessionID,
        Title: "Reasoning Effort Test",
        LargeModelProvider: "local-cli",
        LargeModelID: "cli-claude-opus",
        LargeModelReasoningEffort: "high",
      }),
    });

    // Label should still show H after "reload"
    await expect(
      page.locator('[data-test-id="reasoning-effort-large-label"]'),
    ).toHaveText("H");
  });

  test("effort badge is displayed in assistant message", async ({ page }) => {
    const sessionID = await setup(page);

    // Set effort to max (M → H → X)
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await expect(
      page.locator('[data-test-id="reasoning-effort-large-label"]'),
    ).toHaveText("X");

    // Send a message
    await page.locator('[data-test-id="chat-input-textarea"]').fill("Hello");
    await page.locator('[data-test-id="chat-input-send-button"]').click();

    // Wait for send_message
    await waitForWSSend(page, "send_message");

    // Send a mock assistant response with reasoning effort
    await sendMockWSMessage(page, {
      type: "message_created",
      payload: makeMessage({
        ID: "resp-max",
        SessionID: sessionID,
        Role: "assistant",
        Parts: [{ type: "text", Text: "Response with max effort" }],
        Model: "cli-claude-opus",
        Provider: "local-cli",
        ReasoningEffort: "max",
      }),
    });

    // Wait for message text to appear
    await expect(page.getByText("Response with max effort")).toBeVisible({ timeout: 3000 });

    // Check that the model badge shows effort level X
    // The badge is a small span with the effort letter next to the model name
    const effortBadge = page.locator("span[title='Reasoning effort: max']");
    await expect(effortBadge).toBeVisible();
    await expect(effortBadge).toHaveText("X");
  });

  test("changing small model effort works independently", async ({ page }) => {
    await setup(page);

    const largeLabel = page.locator('[data-test-id="reasoning-effort-large-label"]');
    const smallLabel = page.locator('[data-test-id="reasoning-effort-small-label"]');

    // Set large to high
    await page.locator('[data-test-id="reasoning-effort-large-increase"]').click();
    await expect(largeLabel).toHaveText("H");
    await expect(smallLabel).toHaveText("M"); // Small unchanged

    // Set small to max
    await page.locator('[data-test-id="reasoning-effort-small-increase"]').click();
    await page.locator('[data-test-id="reasoning-effort-small-increase"]').click();
    await expect(smallLabel).toHaveText("X");
    await expect(largeLabel).toHaveText("H"); // Large unchanged
  });
});
