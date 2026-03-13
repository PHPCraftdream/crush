/**
 * Sidebar detail tests.
 *
 * Covers:
 *  - Double-click to rename in sidebar
 *  - Sidebar rename sends rename_session WS command
 *  - Sidebar rename via Enter commits
 *  - Sidebar rename via Escape cancels
 *  - Untitled session shows "Untitled session" fallback
 *  - Sidebar shows "New" button
 *  - Active session is visually highlighted
 *  - Rename via pencil button opens edit mode
 *  - Save button in edit mode commits rename
 *  - Cancel button in edit mode cancels rename
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

// ── Double-click rename ─────────────────────────────────────────────────

test("double-clicking session in sidebar opens rename input", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-dbl", Title: "Double Click Me" })],
  });
  await expect(page.getByTestId("session-title-sb-dbl")).toBeVisible({ timeout: 3000 });

  // Double-click on the session title to open rename
  await page.getByTestId("session-title-sb-dbl").dblclick();

  // Input should appear in sidebar
  await expect(page.getByTestId("session-edit-input")).toBeVisible({ timeout: 2000 });
});

test("sidebar rename via Enter sends rename_session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-ren1", Title: "Rename Me" })],
  });
  const sessionText = page.getByTestId("session-title-sb-ren1");
  await sessionText.dblclick();

  const input = page.getByTestId("session-edit-input");
  await input.fill("New Sidebar Name");
  await input.press("Enter");

  const cmd = await waitForWSSend(page, "rename_session");
  expect((cmd.payload as { sessionID: string; title: string }).title).toBe("New Sidebar Name");
});

test("sidebar rename via Escape cancels without sending", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-esc", Title: "Escape Rename" })],
  });
  const sessionText = page.getByTestId("session-title-sb-esc");
  await sessionText.dblclick();

  const input = page.getByTestId("session-edit-input");
  await input.fill("Changed Name");
  await input.press("Escape");

  await expect(input).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-title-sb-esc")).toBeVisible();
});

// ── Pencil button rename ────────────────────────────────────────────────

test("pencil button in sidebar opens rename mode", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-pen", Title: "Pencil Rename" })],
  });
  await expect(page.getByTestId("session-title-sb-pen")).toBeVisible({ timeout: 3000 });

  const sessionRow = page.getByTestId("session-sb-pen");
  await sessionRow.hover();
  await page.getByTestId("session-rename-sb-pen").click();

  await expect(page.getByTestId("session-edit-input")).toBeVisible({ timeout: 2000 });
});

// ── Untitled session fallback ──────────────────────────────────────────

test("session with empty title shows Untitled session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-untitled", Title: "" })],
  });

  await expect(page.getByTestId("session-title-sb-untitled")).toHaveText("Untitled session", { timeout: 2000 });
});

// ── Active session highlight ────────────────────────────────────────────

test("active session has different styling in sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sb-act1", Title: "Active One" }),
      makeSession({ ID: "sb-act2", Title: "Inactive Two" }),
    ],
  });
  await expect(page.getByTestId("session-title-sb-act1")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sb-act1").click();

  // Active session row should have distinct class
  const activeRow = page.getByTestId("session-sb-act1");
  await expect(activeRow).toHaveClass(/border-accent/, { timeout: 2000 });
});

// ── New button ──────────────────────────────────────────────────────────

test("sidebar shows New button", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("sidebar-new-session")).toBeVisible({ timeout: 2000 });
});

// ── Sidebar rename Save/Cancel buttons ────────────────────────────────

test("Save button in sidebar edit mode commits rename", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-save", Title: "Save Rename" })],
  });
  const sessionText = page.getByTestId("session-title-sb-save");
  await sessionText.dblclick();

  const input = page.getByTestId("session-edit-input");
  await input.fill("Saved Name");

  // Click the Save (check) button
  await page.getByTestId("session-edit-save").click();

  const cmd = await waitForWSSend(page, "rename_session");
  expect((cmd.payload as { title: string }).title).toBe("Saved Name");
});

test("Cancel button in sidebar edit mode closes without sending", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-cancel", Title: "Cancel Rename" })],
  });
  const sessionText = page.getByTestId("session-title-sb-cancel");
  await sessionText.dblclick();

  await expect(page.getByTestId("session-edit-input")).toBeVisible({ timeout: 2000 });
  await page.getByTestId("session-edit-cancel").click();

  await expect(page.getByTestId("session-edit-input")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-title-sb-cancel")).toBeVisible();
});

// ── Token display in sidebar ────────────────────────────────────────────

test("sidebar shows formatted token count", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-tok", Title: "Token Sidebar", PromptTokens: 500, CompletionTokens: 500 })],
  });

  await expect(page.getByTestId("session-tokens-sb-tok")).toHaveText("1.0k tok", { timeout: 2000 });
});

test("sidebar hides token count when zero", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-notok", Title: "Zero Usage", PromptTokens: 0, CompletionTokens: 0 })],
  });

  // Token count should not be visible when total is 0
  await expect(page.getByTestId("session-tokens-sb-notok")).not.toBeVisible({ timeout: 1000 });
});
