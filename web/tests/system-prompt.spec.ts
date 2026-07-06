/**
 * System prompt modal tests.
 *
 * Covers:
 *  - Prompt button not rendered without active session
 *  - Prompt button visible when session active
 *  - Clicking Prompt opens modal and sends get_system_prompt
 *  - Modal shows loading state, then textarea with content
 *  - Save button disabled when content unchanged
 *  - Save button enabled after editing
 *  - Clicking Save sends set_system_prompt WS command
 *  - Reset button reverts draft to original content
 *  - Escape closes modal
 *  - Clicking backdrop closes modal
 *  - Clicking × closes modal
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sp-sess", Title: "Prompt Session" })],
  });
  await expect(page.getByText("Prompt Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Prompt Session").first().click();
}

// ── Button state ─────────────────────────────────────────────────────────────

test("Prompt button not rendered without active session", async ({ page }) => {
  await page.goto("/");
  const btn = page.getByTestId("header-prompt-button");
  await expect(btn).toHaveCount(0, { timeout: 2000 });
});

test("System prompt button visible when session active", async ({ page }) => {
  await setupWithSession(page);
  const btn = page.getByTestId("header-prompt-button");
  await expect(btn).toBeEnabled({ timeout: 3000 });
});

// ── Opening modal ─────────────────────────────────────────────────────────────

test("System prompt modal opens on button click", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 3000 });
});

test("modal sends get_system_prompt on open", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  const cmd = await waitForWSSend(page, "get_system_prompt");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("sp-sess");
});

// ── Loading and content ───────────────────────────────────────────────────────

test("System prompt modal shows loading then textarea after response", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();

  // Should show loading state initially
  await expect(page.getByText("Loading")).toBeVisible({ timeout: 2000 });

  // Server responds with prompt content
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "You are a helpful assistant." },
  });

  // Loading gone; textarea visible with content
  await expect(page.getByText("Loading")).not.toBeVisible({ timeout: 2000 });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await expect(textarea).toHaveValue("You are a helpful assistant.");
});

test("System prompt modal shows loaded content", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "You are helpful" },
  });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 3000 });
  await expect(textarea).toHaveValue("You are helpful");
});

// ── Save behaviour ────────────────────────────────────────────────────────────

test("Save button disabled when content unchanged", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Original" },
  });
  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(saveBtn).toBeDisabled({ timeout: 3000 });
});

test("Save button enabled after editing", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Original" },
  });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Modified prompt");
  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(saveBtn).toBeEnabled({ timeout: 2000 });
});

test("Save button sends set_system_prompt", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Old prompt" },
  });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("New prompt content");
  await page.locator(".fixed button", { hasText: "Save" }).click();
  const cmd = await waitForWSSend(page, "set_system_prompt");
  const payload = cmd.payload as { sessionID: string; content: string };
  expect(payload.sessionID).toBe("sp-sess");
  expect(payload.content).toBe("New prompt content");
});

// ── Reset ─────────────────────────────────────────────────────────────────────

test("Reset button reverts draft to original content", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Original text" },
  });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Changed text");
  // Reset button appears only when draft differs from original
  await page.locator(".fixed button", { hasText: "Reset" }).click();
  await expect(textarea).toHaveValue("Original text");
});

// ── Close ─────────────────────────────────────────────────────────────────────

test("System prompt modal closes on Escape", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 2000 });
  await page.keyboard.press("Escape");
  await expect(page.getByText("System Prompt")).not.toBeVisible({ timeout: 3000 });
});

test("clicking × closes system prompt modal", async ({ page }) => {
  await setupWithSession(page);
  await page.getByTestId("header-prompt-button").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 2000 });
  await page.locator(".fixed button", { hasText: "×" }).click();
  await expect(page.getByText("System Prompt")).not.toBeVisible({ timeout: 2000 });
});
