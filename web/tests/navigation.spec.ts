/**
 * Navigation and hash routing tests.
 *
 * Covers:
 *  - Auto-session creation when session list is empty
 *  - Hash contains session ID after selecting
 *  - Hash-based session restore on page load
 *  - Auto-selects most recent session when no valid hash
 *  - Clearing active session clears hash
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

// ── Auto-session creation ──────────────────────────────────────────────

test("auto-creates session when sessions_list is empty", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });

  const cmd = await waitForWSSend(page, "create_session");
  expect(cmd.type).toBe("create_session");
});

// ── Hash routing ────────────────────────────────────────────────────────

test("selecting a session updates URL hash", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "nav-1", Title: "Nav Session" }),
      makeSession({ ID: "nav-2", Title: "Other Nav" }),
    ],
  });
  await expect(page.getByText("Nav Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Nav Session").first().click();

  // Hash should contain the session ID
  const url = page.url();
  expect(url).toContain("#/nav-1");
});

test("auto-selects most recent session (first in list) when no hash", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "auto-1", Title: "Most Recent" }),
      makeSession({ ID: "auto-2", Title: "Older Session" }),
    ],
  });

  // Should auto-select first session and load messages
  const cmd = await waitForWSSend(page, "load_messages");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("auto-1");
});

test("hash-based session restore loads correct session", async ({ page }) => {
  // Navigate with hash already set
  await page.goto("/#/hash-sess");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "hash-sess", Title: "Hash Session" }),
      makeSession({ ID: "other-sess", Title: "Other" }),
    ],
  });

  // Should load messages for the hash session
  const cmd = await waitForWSSend(page, "load_messages");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("hash-sess");

  // Header should show the correct session
  await expect(
    page.locator("header h1").getByText("Hash Session")
  ).toBeVisible({ timeout: 2000 });
});

test("invalid hash falls back to most recent session", async ({ page }) => {
  await page.goto("/#/nonexistent-id");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "fallback-1", Title: "Fallback Session" }),
    ],
  });

  // Should fall back to first session
  const cmd = await waitForWSSend(page, "load_messages");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("fallback-1");
});

// ── Session created auto-select ─────────────────────────────────────────

test("session_created event auto-selects the new session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "old-sess", Title: "Old Session" })],
  });

  // Server creates new session
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "new-auto", Title: "New Auto Session" }),
  });

  // Should auto-select and show in header
  await expect(
    page.locator("header h1").getByText("New Auto Session")
  ).toBeVisible({ timeout: 2000 });

  // Hash should update
  const url = page.url();
  expect(url).toContain("#/new-auto");
});

// ── Session deletion clears selection ───────────────────────────────────

test("deleting active session clears hash and shows no session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "del-active", Title: "Active To Delete" })],
  });
  await expect(page.getByText("Active To Delete").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Active To Delete").first().click();

  // Server deletes the active session
  await sendMockWSMessage(page, {
    type: "session_deleted",
    payload: { ID: "del-active" },
  });

  // Header should show "No session selected"
  await expect(
    page.locator("header h1").getByText("No session selected")
  ).toBeVisible({ timeout: 2000 });
});
