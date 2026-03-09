/**
 * Batch message selection tests.
 *
 * Covers:
 *  - Checkbox appears on message hover
 *  - Selecting a message shows batch toolbar with count
 *  - Selecting multiple messages updates count
 *  - "Delete selected" triggers confirm dialog with count
 *  - Confirming sends delete_messages WS command with all IDs
 *  - "Cancel" in toolbar clears selection
 *  - Selection clears when switching sessions
 *  - Toggling checkbox deselects message
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithMessages(
  page: import("@playwright/test").Page,
  sessionID: string,
  messages: ReturnType<typeof makeMessage>[]
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: sessionID, Title: "Batch Session" }),
      makeSession({ ID: "batch-other", Title: "Other Session" }),
    ],
  });
  await expect(page.getByText("Batch Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Batch Session").first().click();
  await sendMockWSMessage(page, { type: "messages_list", payload: messages });
}

const twoMessages = [
  makeMessage({ ID: "b-m1", Role: "user", Parts: [{ type: "text", Text: "First batch msg" }] }),
  makeMessage({ ID: "b-m2", Role: "assistant", Parts: [{ type: "text", Text: "Second batch msg" }] }),
];

// ── Checkbox visibility ─────────────────────────────────────────────────

test("checkbox appears on message hover", async ({ page }) => {
  await setupWithMessages(page, "batch-1", twoMessages);
  await expect(page.getByText("First batch msg")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await expect(msgRow.locator("input[type='checkbox']")).toBeVisible({ timeout: 2000 });
});

// ── Selection toolbar ───────────────────────────────────────────────────

test("selecting a message shows batch toolbar with count", async ({ page }) => {
  await setupWithMessages(page, "batch-2", twoMessages);

  const msgRow = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await msgRow.locator("input[type='checkbox']").click();

  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Delete selected")).toBeVisible();
});

test("selecting multiple messages updates count", async ({ page }) => {
  await setupWithMessages(page, "batch-3", twoMessages);

  // Select first message
  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row1.hover();
  await row1.locator("input[type='checkbox']").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Select second message
  const row2 = page.getByText("Second batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row2.hover();
  await row2.locator("input[type='checkbox']").click();
  await expect(page.getByText("2 selected")).toBeVisible({ timeout: 2000 });
});

// ── Batch delete ────────────────────────────────────────────────────────

test("Delete selected triggers confirm dialog with count", async ({ page }) => {
  await setupWithMessages(page, "batch-4", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row1.hover();
  await row1.locator("input[type='checkbox']").click();

  const row2 = page.getByText("Second batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row2.hover();
  await row2.locator("input[type='checkbox']").click();

  await page.getByText("Delete selected").click();
  await expect(page.getByText("Delete 2 selected messages?")).toBeVisible({ timeout: 2000 });
});

test("confirming batch delete sends delete_messages with all IDs", async ({ page }) => {
  await setupWithMessages(page, "batch-5", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row1.hover();
  await row1.locator("input[type='checkbox']").click();

  const row2 = page.getByText("Second batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row2.hover();
  await row2.locator("input[type='checkbox']").click();

  await page.getByText("Delete selected").click();
  await page.getByRole("button", { name: "Delete", exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_messages");
  const ids = (cmd.payload as { messageIDs: string[] }).messageIDs;
  expect(ids).toContain("b-m1");
  expect(ids).toContain("b-m2");
});

// ── Cancel selection ────────────────────────────────────────────────────

test("Cancel in toolbar clears selection and hides toolbar", async ({ page }) => {
  await setupWithMessages(page, "batch-6", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row1.hover();
  await row1.locator("input[type='checkbox']").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Click Cancel in the batch toolbar (not the confirm dialog)
  await page.locator("button").filter({ hasText: /^Cancel$/ }).last().click();
  await expect(page.getByText("1 selected")).not.toBeVisible({ timeout: 2000 });
});

// ── Deselect ─────────────────────────────────────────────────────────────

test("toggling checkbox deselects message", async ({ page }) => {
  await setupWithMessages(page, "batch-7", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row1.hover();
  await row1.locator("input[type='checkbox']").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Click checkbox again to deselect
  await row1.locator("input[type='checkbox']").click();
  await expect(page.getByText("1 selected")).not.toBeVisible({ timeout: 2000 });
});

// ── Selection clears on session switch ──────────────────────────────────

test("selection clears when switching sessions", async ({ page }) => {
  await setupWithMessages(page, "batch-8", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await row1.hover();
  await row1.locator("input[type='checkbox']").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Switch to other session
  await page.getByText("Other Session").click();
  await expect(page.getByText("1 selected")).not.toBeVisible({ timeout: 2000 });
});
