/**
 * Per-session YOLO mode tests.
 *
 * Tests for session-specific YOLO feature:
 *  - set_yolo sends sessionID with active session
 *  - Different sessions can have different YOLO states
 *  - YOLO state persists per session (backend DB)
 *  - Session YoloEnabled field from backend is source of truth
 *  - Permission auto-grant works with per-session YOLO
 *
 * @since 2026-03-12
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function activateSession(page: import("@playwright/test").Page, sessionID: string, sessionTitle: string) {
  // Send sessions list
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: sessionTitle, YoloEnabled: false })],
  });

  // Click session in sidebar using data-test-id
  await page.locator(`[data-test-id="session-${sessionID}"]`).click();

  // Wait for load_messages to be sent (this confirms session activation)
  await waitForWSSend(page, "load_messages");

  // Send empty messages list to complete activation
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [],
  });

  // Wait for ChatToolbar to be visible (it has Yolo button)
  await expect(page.locator('[data-test-id="yolo-button"]')).toBeVisible({ timeout: 3000 });
}

// ── Per-Session YOLO State ───────────────────────────────────────────────────

test("set_yolo command includes sessionID when session is active", async ({ page }) => {
  await page.goto("/");

  // Activate session
  await activateSession(page, "yolo-sess-1", "Yolo Session 1");

  // Enable YOLO
  const yoloBtn = page.locator('[data-test-id="yolo-button"]').first();
  await clearWSSent(page);  // Clear any previous set_yolo messages
  await yoloBtn.click();
  const cmd = await waitForWSSend(page, "set_yolo");

  expect(cmd.payload).toHaveProperty("sessionID", "yolo-sess-1");
  expect((cmd.payload as { sessionID: string; enabled: boolean }).enabled).toBe(true);
});

test("different sessions can have different YOLO states", async ({ page }) => {
  await page.goto("/");

  // Activate Session A (YOLO off)
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-sess-a", Title: "Session A", YoloEnabled: false }),
      makeSession({ ID: "yolo-sess-b", Title: "Session B", YoloEnabled: true }),
    ],
  });

  await page.locator('[data-test-id="session-yolo-sess-a"]').click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn = page.locator('[data-test-id="yolo-button"]').first();
  await expect(yoloBtn).toBeVisible({ timeout: 3000 });

  // Enable YOLO in Session A
  await clearWSSent(page);  // Clear any previous set_yolo messages
  await yoloBtn.click();
  await waitForWSSend(page, "set_yolo");

  // Simulate backend update
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "yolo-sess-a", Title: "Session A", YoloEnabled: true }),
  });

  // Check YOLO button has active class (yellow background)
  await expect(yoloBtn).toHaveClass(/bg-yellow/, { timeout: 2000 });

  // Switch to Session B (which has YOLO enabled from backend)
  await page.locator('[data-test-id="session-yolo-sess-b"]').click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn2 = page.locator('[data-test-id="yolo-button"]').first();
  // Session B should also show YOLO active (from its YoloEnabled state)
  await expect(yoloBtn2).toHaveClass(/bg-yellow/, { timeout: 2000 });
});

test("switching sessions updates YOLO state based on session", async ({ page }) => {
  await page.goto("/");

  // Session A: YOLO off, Session B: YOLO on
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-switch-a", Title: "Session A (YOLO OFF)", YoloEnabled: false }),
      makeSession({ ID: "yolo-switch-b", Title: "Session B (YOLO ON)", YoloEnabled: true }),
    ],
  });

  // Start with Session A (YOLO off)
  await page.locator('[data-test-id="session-yolo-switch-a"]').click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn = page.locator('[data-test-id="yolo-button"]').first();
  await expect(yoloBtn).toBeVisible({ timeout: 3000 });

  // YOLO button should NOT have active class
  await expect(yoloBtn).not.toHaveClass(/bg-yellow/, { timeout: 2000 });

  // Switch to Session B (YOLO on)
  await page.locator('[data-test-id="session-yolo-switch-b"]').click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn2 = page.locator('[data-test-id="yolo-button"]').first();
  // YOLO button should have active class
  await expect(yoloBtn2).toHaveClass(/bg-yellow/, { timeout: 2000 });

  // Switch back to Session A
  await page.locator('[data-test-id="session-yolo-switch-a"]').click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn3 = page.locator('[data-test-id="yolo-button"]').first();
  // YOLO button should NOT have active class
  await expect(yoloBtn3).not.toHaveClass(/bg-yellow/, { timeout: 2000 });
});

// ── Backend YoloEnabled Field ─────────────────────────────────────────────────

test("session with YoloEnabled=true from backend shows active YOLO button", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-backend-on", Title: "YOLO Session", YoloEnabled: true })],
  });

  // Click session to activate
  await page.locator('button:has-text("YOLO Session")').first().click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn = page.locator('[data-test-id="yolo-button"]').first();
  // YOLO button should have active class
  await expect(yoloBtn).toHaveClass(/bg-yellow/, { timeout: 2000 });
});

test("session with YoloEnabled=false from backend shows inactive YOLO button", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-backend-off", Title: "No YOLO Session", YoloEnabled: false })],
  });

  // Click session to activate
  await page.locator('button:has-text("No YOLO Session")').first().click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn = page.locator('[data-test-id="yolo-button"]').first();
  // YOLO button should NOT have active class
  await expect(yoloBtn).not.toHaveClass(/bg-yellow/, { timeout: 2000 });
});

test("session_updated event with YoloEnabled change updates UI", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-update", Title: "Update YOLO", YoloEnabled: false })],
  });

  await page.locator('button:has-text("Update YOLO")').first().click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn = page.locator('[data-test-id="yolo-button"]').first();
  // YOLO button should NOT have active class
  await expect(yoloBtn).not.toHaveClass(/bg-yellow/, { timeout: 2000 });

  // Backend updates YOLO state
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "yolo-update", Title: "Update YOLO", YoloEnabled: true }),
  });

  // UI should update to show active YOLO button
  await expect(yoloBtn).toHaveClass(/bg-yellow/, { timeout: 2000 });
});

// ── Permission Auto-Grant with Per-Session YOLO ─────────────────────────────

test("permission_request is auto-granted when session YOLO is enabled", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-perm", Title: "YOLO Perm Session", YoloEnabled: true })],
  });

  await page.locator('button:has-text("YOLO Perm Session")').first().click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  // Send permission request
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "perm-auto",
      SessionID: "yolo-perm",
      ToolCallID: "tc-auto",
      ToolName: "bash",
      Description: "Auto-grant this",
      Action: "execute",
      Path: "/tmp/auto.sh",
      Params: {},
    },
  });

  // Should auto-grant (send grant_permission)
  // Wait a bit for the auto-grant to happen
  await page.waitForTimeout(100);

  const granted = await page.evaluate(() => {
    const sent = (window as unknown as Record<string, unknown>).__wsSent as Array<{ type: string }>;
    return sent.some((m) => m.type === "grant_permission");
  });

  expect(granted).toBe(true);

  // Permission dialog should NOT be shown
  await expect(page.getByText("bash")).not.toBeVisible({ timeout: 1000 });
});

test("permission_request shows dialog when session YOLO is disabled", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-no-perm", Title: "No YOLO Perm", YoloEnabled: false })],
  });

  await page.locator('button:has-text("No YOLO Perm")').first().click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  // Send permission request
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "perm-manual",
      SessionID: "yolo-no-perm",
      ToolCallID: "tc-manual",
      ToolName: "write_file",
      Description: "Manual approval needed",
      Action: "write",
      Path: "/tmp/manual.txt",
      Params: {},
    },
  });

  // Should show permission dialog
  await expect(page.getByText("write_file")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Manual approval needed")).toBeVisible();
});

// ── Multiple Sessions with Different YOLO States ───────────────────────────

test("permission request in non-YOLO session while another has YOLO", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-multi-a", Title: "YOLO Session", YoloEnabled: true }),
      makeSession({ ID: "yolo-multi-b", Title: "Non-YOLO Session", YoloEnabled: false }),
    ],
  });

  // Activate non-YOLO session
  await page.locator('[data-test-id="session-yolo-multi-b"]').click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  // Send permission for this session
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "perm-multi",
      SessionID: "yolo-multi-b",
      ToolCallID: "tc-multi",
      ToolName: "bash",
      Description: "Multi session perm",
      Action: "execute",
      Path: "/tmp/multi.sh",
      Params: {},
    },
  });

  // Should show dialog (this session doesn't have YOLO)
  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
});

// ── YOLO State Persistence Across Page Reload ───────────────────────────────

test("YOLO state persists across page reload (from backend)", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-reload", Title: "Reload YOLO", YoloEnabled: false })],
  });

  await page.locator('button:has-text("Reload YOLO")').first().click();
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn = page.locator('[data-test-id="yolo-button"]').first();

  // Enable YOLO
  await clearWSSent(page);  // Clear any previous set_yolo messages
  await yoloBtn.click();
  await waitForWSSend(page, "set_yolo");

  // Simulate backend update
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "yolo-reload", Title: "Reload YOLO", YoloEnabled: true }),
  });

  // YOLO button should have active class
  await expect(yoloBtn).toHaveClass(/bg-yellow/, { timeout: 2000 });

  // Reload page
  await setupMockWS(page);
  await page.reload();
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );

  // Re-send sessions with YOLO enabled from backend
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-reload", Title: "Reload YOLO", YoloEnabled: true })],
  });

  // Session should be auto-activated on reload
  await waitForWSSend(page, "load_messages");
  await sendMockWSMessage(page, { type: "messages_list", payload: [] });

  const yoloBtn2 = page.locator('[data-test-id="yolo-button"]').first();
  // YOLO should still be ON (restored from backend)
  await expect(yoloBtn2).toHaveClass(/bg-yellow/, { timeout: 3000 });
});
