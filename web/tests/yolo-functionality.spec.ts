/**
 * YOLO functionality integration tests
 *
 * Tests that YOLO mode:
 * - Can be toggled via PermissionsModal
 * - Persists to localStorage
 * - Restores correctly when switching sessions
 * - Sends correct WebSocket messages
 * - Affects permission requests
 *
 * @since 2026-03-12
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig, makePermission } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  // Clear localStorage before each test
  await page.addInitScript(() => {
    localStorage.removeItem("crush_yolo");
    localStorage.removeItem("crush_yolo_sessions");
  });
});

// ── YOLO Toggle in PermissionsModal ────────────────────────────────────────────────

test("opening PermissionsModal shows current YOLO state", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session", YoloEnabled: false })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Open permissions modal via YOLO button
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Should show YOLO is OFF
  await expect(page.getByText("YOLO Mode")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible();
});

test("toggling YOLO in modal sends set_yolo message", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session" })],
  });
  await page.getByText("Test Session").first().click();

  // Wait for session to be selected (check for active styling)
  await expect(page.locator(".bg-canvas.border-accent\\/20")).toBeVisible({ timeout: 2000 });

  // Open permissions modal
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Wait for modal to open
  await expect(page.getByText("YOLO Mode")).toBeVisible({ timeout: 2000 });

  // Click YOLO toggle
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();

  // Wait for UI to update
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible({ timeout: 2000 });

  // Should send set_yolo message
  const cmd = await waitForWSSend(page, "set_yolo");
  expect((cmd.payload as { enabled: boolean }).enabled).toBe(true);
});

test("toggling YOLO updates UI immediately", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session" })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Open permissions modal
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Click YOLO toggle
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();

  // Should update text immediately
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible({ timeout: 1000 });

  // Toggle back
  await toggle.click();
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible({ timeout: 1000 });
});

// ── localStorage Persistence ───────────────────────────────────────────────────────

test("YOLO state persists to localStorage crush_yolo", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session" })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Open permissions modal and enable YOLO
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();
  await waitForWSSend(page, "set_yolo");

  // Check localStorage
  const globalYolo = await page.evaluate(() => localStorage.getItem("crush_yolo"));
  expect(globalYolo).toBe("true");
});

test("YOLO state persists per-session to localStorage", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Session 1" })],
  });
  await page.getByText("Session 1").first().click();
  await page.waitForTimeout(200);

  // Enable YOLO
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();
  await waitForWSSend(page, "set_yolo");

  // Check per-session localStorage
  const sessionYolo = await page.evaluate(() => localStorage.getItem("crush_yolo_sessions"));
  expect(sessionYolo).toBe(JSON.stringify({ "sess-1": true }));
});

// ── Session Switching ───────────────────────────────────────────────────────────────

test("switching sessions restores YOLO state from localStorage", async ({ page }) => {
  await page.goto("/");

  // Setup sessions
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sess-a", Title: "Session A" }),
      makeSession({ ID: "sess-b", Title: "Session B" }),
    ],
  });

  // Set Session A YOLO = true
  await page.getByText("Session A").first().click();
  await page.waitForTimeout(200);
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();
  await waitForWSSend(page, "set_yolo");
  await page.locator("button").filter({ hasText: "Close" }).click();

  // Switch to Session B
  await page.getByText("Session B").first().click();
  await page.waitForTimeout(200);

  // Open modal for Session B
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Session B should have YOLO OFF by default
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible({ timeout: 2000 });

  // Switch back to Session A (without closing modal)
  await page.getByText("Session A").first().click();
  await page.waitForTimeout(200);

  // Open modal for Session A
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Session A should still have YOLO ON
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible({ timeout: 2000 });
});

test("switching sessions sends correct set_yolo message", async ({ page }) => {
  // Pre-populate localStorage before navigation
  await page.goto("/");

  // First enable YOLO for Session A
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sess-a", Title: "Session A" }),
      makeSession({ ID: "sess-b", Title: "Session B" }),
    ],
  });

  // Enable YOLO for Session A
  await page.getByText("Session A").first().click();
  await page.waitForTimeout(200);
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await page.locator(".relative.w-14.h-7").first().click();
  await waitForWSSend(page, "set_yolo");
  await page.locator("button").filter({ hasText: "Close" }).click();

  // Now switch to Session B
  await page.getByText("Session B").first().click();
  await page.waitForTimeout(200);

  // Should send set_yolo with enabled: false (default for Session B)
  const cmd = await waitForWSSend(page, "set_yolo");
  expect((cmd.payload as { sessionID: string; enabled: boolean }).sessionID).toBe("sess-b");
  expect((cmd.payload as { enabled: boolean }).enabled).toBe(false);

  // Switch back to Session A
  await page.getByText("Session A").first().click();
  await page.waitForTimeout(200);

  // Should restore YOLO state from localStorage
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible({ timeout: 2000 });
});

// ── Permission Requests ──────────────────────────────────────────────────────────────

test("when YOLO is enabled, permission requests are auto-granted", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session" })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Enable YOLO
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();
  await waitForWSSend(page, "set_yolo");
  await page.locator("button").filter({ hasText: "Close" }).click();

  // Send a permission request from backend
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ToolName: "bash", ToolCallID: "tc-1" }),
  });

  // Should NOT show permission dialog (auto-granted)
  // Instead, check that grant_permission was sent
  const cmd = await waitForWSSend(page, "grant_permission");
  expect((cmd.payload as { permissionID: string }).permissionID).toBe("perm-1");
});

test("when YOLO is disabled, permission requests show dialog", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session" })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Ensure YOLO is OFF
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await page.locator("button").filter({ hasText: "Close" }).click();

  // Send a permission request
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ToolName: "bash", ToolCallID: "tc-2" }),
  });

  // Should show permission dialog
  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
});

// ── Backend Synchronization ────────────────────────────────────────────────────────

test("backend can override YOLO state via config event", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session" })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Backend sends config with yolo: true
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ yolo: true }),
  });

  // Open permissions modal
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Should show YOLO is ON
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible({ timeout: 2000 });
});

test("session_updated event updates YOLO state in modal", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session", YoloEnabled: false })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Open modal
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Backend updates session
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "sess-1", Title: "Test Session", YoloEnabled: true }),
  });

  // Modal should reflect new state
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible({ timeout: 2000 });
});

// ── Toolbar Button State ───────────────────────────────────────────────────────────

test("YOLO button in toolbar reflects current state", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sess-1", Title: "Test Session" })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Should show OFF state (no yellow background)
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButton).not.toHaveClass(/bg-yellow/);

  // Enable YOLO via modal
  await yoloButton.click();
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();
  await waitForWSSend(page, "set_yolo");

  // Close modal
  await page.locator("button").filter({ hasText: "Close" }).click();

  // Toolbar button should now have yellow background
  await expect(yoloButton).toHaveClass(/bg-yellow/);
});
