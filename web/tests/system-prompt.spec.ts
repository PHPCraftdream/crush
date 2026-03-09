/**
 * System prompt modal tests.
 *
 * Covers:
 *  - Prompt button disabled without active session
 *  - Clicking Prompt opens modal
 *  - Modal shows loading state, then textarea
 *  - Editing enables Save button
 *  - Save sends set_system_prompt WS command
 *  - Reset reverts draft to original
 *  - Escape closes modal
 *  - Clicking backdrop closes modal
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sp-sess", Title: "Prompt Session" })],
  });
  await expect(page.getByText("Prompt Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Prompt Session").first().click();
}

// ── Button state ────────────────────────────────────────────────────────

test("Prompt button disabled without active session", async ({ page }) => {
  await page.goto("/");
  const btn = page.locator("header button[title='View / edit system prompt']");
  await expect(btn).toBeDisabled({ timeout: 2000 });
});

test("Prompt button enabled with active session", async ({ page }) => {
  await setupWithSession(page);
  const btn = page.locator("header button[title='View / edit system prompt']");
  await expect(btn).toBeEnabled({ timeout: 2000 });
});

// ── Opening modal ───────────────────────────────────────────────────────

test("clicking Prompt opens system prompt modal", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 2000 });
});

test("modal sends get_system_prompt on open", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  const cmd = await waitForWSSend(page, "get_system_prompt");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("sp-sess");
});

// ── Loading and content ─────────────────────────────────────────────────

test("modal shows Loading state then textarea after system_prompt response", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();

  // Should show loading
  await expect(page.getByText("Loading")).toBeVisible({ timeout: 2000 });

  // Server responds with prompt content
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "You are a helpful assistant." },
  });

  // Loading gone, textarea visible with content
  await expect(page.getByText("Loading")).not.toBeVisible({ timeout: 2000 });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await expect(textarea).toHaveValue("You are a helpful assistant.");
});

// ── Save ────────────────────────────────────────────────────────────────

test("Save button disabled when content unchanged", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Original" } });

  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(saveBtn).toBeDisabled({ timeout: 2000 });
});

test("Save button enabled after editing", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Original" } });

  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Modified prompt");

  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(saveBtn).toBeEnabled({ timeout: 2000 });
});

test("clicking Save sends set_system_prompt", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Old prompt" } });

  const textarea = page.locator(".fixed textarea");
  await textarea.fill("New prompt content");

  await page.locator(".fixed button", { hasText: "Save" }).click();

  const cmd = await waitForWSSend(page, "set_system_prompt");
  const payload = cmd.payload as { sessionID: string; content: string };
  expect(payload.sessionID).toBe("sp-sess");
  expect(payload.content).toBe("New prompt content");
});

// ── Reset ───────────────────────────────────────────────────────────────

test("Reset button reverts draft to original content", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Original text" } });

  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Changed text");

  // Reset button should appear when dirty
  await page.locator(".fixed button", { hasText: "Reset" }).click();

  await expect(textarea).toHaveValue("Original text");
});

// ── Close ───────────────────────────────────────────────────────────────

test("Escape closes system prompt modal", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 2000 });

  await page.keyboard.press("Escape");
  await expect(page.getByText("System Prompt")).not.toBeVisible({ timeout: 2000 });
});

test("clicking × closes system prompt modal", async ({ page }) => {
  await setupWithSession(page);
  await page.locator("header button[title='View / edit system prompt']").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 2000 });

  // Click the × button in the modal header
  await page.locator(".fixed button", { hasText: "×" }).click();
  await expect(page.getByText("System Prompt")).not.toBeVisible({ timeout: 2000 });
});
