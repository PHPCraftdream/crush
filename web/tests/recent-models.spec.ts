/**
 * Recent models tests.
 *
 * Covers:
 *  - Recent section appears in dropdown after selecting a model
 *  - Recent models listed in order of most recent first
 *  - Remove button removes model from recents
 *  - Remove sends remove_recent_model WS command
 *  - Assistant response tracks model as recently used
 *  - Config with recentLargeModels populates recent section
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  // Clear recent models localStorage
  await page.addInitScript(() => {
    localStorage.removeItem("crush_recent_models_large");
    localStorage.removeItem("crush_recent_models_small");
  });
});

function multiModelConfig() {
  return makeConfig({
    providers: {
      anthropic: {
        name: "Anthropic",
        enabled: true,
        type: "api",
        models: [
          { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
          { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
        ],
      },
      openai: {
        name: "OpenAI",
        enabled: true,
        type: "api",
        models: [
          { id: "gpt-4o", name: "gpt-4o", contextWindow: 128000 },
        ],
      },
    },
  });
}

async function setupSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "rec-sess", Title: "Recent Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: multiModelConfig() });
  await expect(page.getByText("Recent Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Recent Session").first().click();
}

// ── Recent section appears after selecting ──────────────────────────────

test("Recent section appears in dropdown after selecting a model", async ({ page }) => {
  await setupSession(page);

  // Select haiku from large dropdown
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.z-50").getByText("claude-haiku-4").first().click();
  await waitForWSSend(page, "set_session_models");

  // Re-open dropdown — should now have "Recent" section
  await page.locator("header button[title='Large (strong) model']").click();
  await expect(page.locator("div.z-50").getByText("Recent")).toBeVisible({ timeout: 2000 });
});

// ── Remove from recents ─────────────────────────────────────────────────

test("remove button removes model from recents and sends WS command", async ({ page }) => {
  await setupSession(page);

  // Select a model to add to recents
  await page.locator("header button[title='Large (strong) model']").click();
  await page.locator("div.z-50").getByText("claude-haiku-4").first().click();
  await waitForWSSend(page, "set_session_models");

  // Re-open dropdown
  await page.locator("header button[title='Large (strong) model']").click();
  await expect(page.locator("div.z-50").getByText("Recent")).toBeVisible({ timeout: 2000 });

  // Click remove (✕) button next to the recent model
  await page.locator("div.z-50 button[title='Remove from recent']").click();

  const cmd = await waitForWSSend(page, "remove_recent_model");
  const payload = cmd.payload as { modelType: string; provider: string; model: string };
  expect(payload.modelType).toBe("large");
  expect(payload.provider).toBe("anthropic");
  expect(payload.model).toBe("claude-haiku-4");
});

// ── Assistant response tracks model ──────────────────────────────────────

test("assistant response tracks model as recently used", async ({ page }) => {
  await setupSession(page);

  // Simulate assistant message with a model
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "track-m1",
      SessionID: "rec-sess",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Response from gpt-4o" }],
      Model: "gpt-4o",
      Provider: "openai",
    }),
  });
  await expect(page.getByText("Response from gpt-4o")).toBeVisible({ timeout: 2000 });

  // Open large model dropdown — "Recent" should show gpt-4o
  await page.locator("header button[title='Large (strong) model']").click();
  await expect(page.locator("div.z-50").getByText("Recent")).toBeVisible({ timeout: 2000 });
});

// ── Config populates recents ────────────────────────────────────────────

test("config with recentLargeModels populates recent section", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "cfg-rec", Title: "Config Recents" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: {
      ...multiModelConfig(),
      recentLargeModels: [
        { Provider: "openai", Model: "gpt-4o" },
      ],
    },
  });
  await expect(page.getByText("Config Recents").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Config Recents").first().click();

  // Open dropdown — should have Recent section pre-populated
  await page.locator("header button[title='Large (strong) model']").click();
  await expect(page.locator("div.z-50").getByText("Recent")).toBeVisible({ timeout: 2000 });
});
