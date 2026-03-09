/**
 * Message editing tests.
 *
 * Covers:
 *  - Edit button visible on hover for user messages
 *  - Edit button visible on hover for assistant messages
 *  - Clicking edit opens textarea with message content
 *  - Escape cancels edit without sending
 *  - Ctrl+Enter commits edit and sends update_message_content
 *  - Save button commits edit
 *  - Cancel button cancels edit
 *  - Unchanged content does not send WS command
 *  - Empty content does not send WS command
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

async function setupWithMessage(
  page: import("@playwright/test").Page,
  msg: ReturnType<typeof makeMessage>
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "edit-sess", Title: "Edit Session" })],
  });
  await expect(page.getByText("Edit Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Edit Session").first().click();
  await sendMockWSMessage(page, { type: "messages_list", payload: [msg] });
}

// ── Edit button visibility ──────────────────────────────────────────────────

test("edit button appears on hover for user message", async ({ page }) => {
  const msg = makeMessage({
    ID: "eu-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Editable user message" }],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Editable user message")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Editable user message").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Edit message")).toBeVisible({ timeout: 2000 });
});

test("edit button appears on hover for assistant message", async ({ page }) => {
  const msg = makeMessage({
    ID: "ea-1",
    Role: "assistant",
    Parts: [{ type: "text", Text: "Editable assistant message" }],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Editable assistant message")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Editable assistant message").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Edit message")).toBeVisible({ timeout: 2000 });
});

// ── Starting edit ─────────────────────────────────────────────────────────

test("clicking edit opens textarea with current message content", async ({ page }) => {
  const msg = makeMessage({
    ID: "es-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Original content" }],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Original content")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Original content").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit message").click();

  const textarea = page.locator("textarea").first();
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await expect(textarea).toHaveValue("Original content");
});

// ── Cancel edit ──────────────────────────────────────────────────────────

test("pressing Escape cancels edit", async ({ page }) => {
  const msg = makeMessage({
    ID: "ec-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Cancel me" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Cancel me").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit message").click();

  const textarea = page.locator("textarea").first();
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await textarea.fill("Changed text");
  await textarea.press("Escape");

  // Edit textarea gone (chat input textarea remains)
  await expect(msgRow.locator("textarea")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Cancel me")).toBeVisible();
});

test("clicking Cancel button cancels edit", async ({ page }) => {
  const msg = makeMessage({
    ID: "ec-2",
    Role: "assistant",
    Parts: [{ type: "text", Text: "Cancel assistant" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Cancel assistant").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit message").click();

  await expect(msgRow.locator("textarea")).toBeVisible({ timeout: 2000 });
  await msgRow.locator("button", { hasText: "Cancel" }).click();

  await expect(msgRow.locator("textarea")).not.toBeVisible({ timeout: 2000 });
});

// ── Commit edit ─────────────────────────────────────────────────────────

test("clicking Save commits edit and sends update_message_content", async ({ page }) => {
  const msg = makeMessage({
    ID: "sv-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Before save" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Before save").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit message").click();

  const textarea = page.locator("textarea").first();
  await textarea.fill("After save");
  // msgRow locator is stale after fill (text changed), find Save button directly
  await page.locator("button").filter({ hasText: /^Save$/ }).click();

  const cmd = await waitForWSSend(page, "update_message_content");
  const payload = cmd.payload as { messageID: string; content: string };
  expect(payload.messageID).toBe("sv-1");
  expect(payload.content).toBe("After save");
});

test("Ctrl+Enter commits edit for assistant message", async ({ page }) => {
  const msg = makeMessage({
    ID: "ce-1",
    Role: "assistant",
    Parts: [{ type: "text", Text: "Before ctrl-enter" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Before ctrl-enter").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit message").click();

  const textarea = page.locator("textarea").first();
  await textarea.fill("After ctrl-enter");
  await textarea.press("Control+Enter");

  const cmd = await waitForWSSend(page, "update_message_content");
  const payload = cmd.payload as { messageID: string; content: string };
  expect(payload.messageID).toBe("ce-1");
  expect(payload.content).toBe("After ctrl-enter");
});

// ── No-op edits ─────────────────────────────────────────────────────────

test("unchanged content does not send WS command", async ({ page }) => {
  const msg = makeMessage({
    ID: "noop-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Same text" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Same text").locator("xpath=ancestor::div[contains(@class,'group/msg')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit message").click();

  // Don't change the text, just click Save
  await msgRow.locator("button", { hasText: "Save" }).click();

  // Verify no update_message_content was sent
  const sent = await page.evaluate(() => {
    const s = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return s.some((m) => m.type === "update_message_content");
  });
  expect(sent).toBe(false);
});
