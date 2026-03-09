/**
 * API key management tests.
 *
 * Covers:
 *  - Disabled provider shows "+ add key" hint
 *  - Clicking disabled provider opens API key form
 *  - Entering and saving API key sends set_provider_key
 *  - Escape cancels API key form
 *  - Enabled provider shows "Edit key" and "Remove key" buttons
 *  - Clicking "Remove key" sends remove_provider_key
 *  - "Edit key" opens API key form
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

function configWithDisabledProvider() {
  return makeConfig({
    providers: {
      anthropic: {
        name: "Anthropic",
        enabled: true,
        type: "api",
        models: [
          { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
        ],
      },
      openai: {
        name: "OpenAI",
        enabled: false,
        type: "api",
        models: [
          { id: "gpt-4o", name: "gpt-4o", contextWindow: 128000 },
        ],
      },
    },
  });
}

const dropdown = (page: import("@playwright/test").Page) =>
  page.locator('[data-testid="model-dropdown"]');

async function openLargeModelDropdown(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "api-sess", Title: "API Key Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: configWithDisabledProvider() });
  await expect(page.getByText("API Key Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("API Key Session").first().click();
  await expect(page.locator("button[title='Large (strong) model']")).toBeVisible({ timeout: 3000 });
  await page.locator("button[title='Large (strong) model']").click();
  await expect(page.getByPlaceholder("Search models…")).toBeVisible({ timeout: 2000 });
}

// ── Disabled provider ──────────────────────────────────────────────────

test("disabled provider model shows add key hint", async ({ page }) => {
  await openLargeModelDropdown(page);
  await expect(dropdown(page).getByText("+ add key")).toBeVisible({ timeout: 2000 });
});

test("clicking disabled model opens API key form", async ({ page }) => {
  await openLargeModelDropdown(page);
  // Click the gpt-4o model (disabled provider)
  await dropdown(page).getByText("gpt-4o").click();
  await expect(page.getByText("Enter API key")).toBeVisible({ timeout: 2000 });
  await expect(page.getByPlaceholder("sk-…")).toBeVisible();
});

test("saving API key sends set_provider_key", async ({ page }) => {
  await openLargeModelDropdown(page);
  await dropdown(page).getByText("gpt-4o").click();
  await expect(page.getByPlaceholder("sk-…")).toBeVisible({ timeout: 2000 });

  await page.getByPlaceholder("sk-…").fill("sk-test-key-123");
  await dropdown(page).locator("button", { hasText: "Save" }).click();

  const cmd = await waitForWSSend(page, "set_provider_key");
  const payload = cmd.payload as { providerID: string; apiKey: string };
  expect(payload.providerID).toBe("openai");
  expect(payload.apiKey).toBe("sk-test-key-123");
});

test("Escape cancels API key form", async ({ page }) => {
  await openLargeModelDropdown(page);
  await dropdown(page).getByText("gpt-4o").click();
  await expect(page.getByText("Enter API key")).toBeVisible({ timeout: 2000 });

  await dropdown(page).locator("button", { hasText: "Cancel" }).click();
  await expect(page.getByText("Enter API key")).not.toBeVisible({ timeout: 2000 });
});

test("Enter key in API key input saves", async ({ page }) => {
  await openLargeModelDropdown(page);
  await dropdown(page).getByText("gpt-4o").click();
  await page.getByPlaceholder("sk-…").fill("sk-enter-key");
  await page.getByPlaceholder("sk-…").press("Enter");

  const cmd = await waitForWSSend(page, "set_provider_key");
  expect((cmd.payload as { apiKey: string }).apiKey).toBe("sk-enter-key");
});

// ── Enabled provider key management ─────────────────────────────────────

test("enabled provider shows Edit key and Remove key buttons", async ({ page }) => {
  await openLargeModelDropdown(page);
  // Anthropic is enabled — should show management buttons
  await expect(dropdown(page).getByText("Edit key")).toBeVisible({ timeout: 2000 });
  await expect(dropdown(page).getByText("Remove key")).toBeVisible();
});

test("clicking Remove key sends remove_provider_key", async ({ page }) => {
  await openLargeModelDropdown(page);
  await dropdown(page).getByText("Remove key").click();

  const cmd = await waitForWSSend(page, "remove_provider_key");
  expect((cmd.payload as { providerID: string }).providerID).toBe("anthropic");
});

test("clicking Edit key opens API key form for enabled provider", async ({ page }) => {
  await openLargeModelDropdown(page);
  await dropdown(page).getByText("Edit key").click();

  await expect(page.getByText("Enter API key")).toBeVisible({ timeout: 2000 });
  await expect(page.getByPlaceholder("sk-…")).toBeVisible();
});

// ── Add API key button for disabled provider group ──────────────────────

test("+ Add API key button opens form for disabled provider group", async ({ page }) => {
  await openLargeModelDropdown(page);
  await dropdown(page).getByText("+ Add API key").click();
  await expect(page.getByText("Enter API key")).toBeVisible({ timeout: 2000 });
});
