/**
 * Persistence and restoration tests for settings.
 *
 * Tests that settings are properly saved and restored:
 *  - Per-session YOLO state persists to backend (DB)
 *  - Per-session YOLO state restored on session switch
 *  - Permission rules persist to backend and are restored
 *  - Permission rules restored when opening modal
 *  - Backend YoloEnabled field is used for restoration
 *  - Settings survive page reload
 *
 * @since 2026-03-12
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

// ── YOLO Persistence (Backend only) ─────────────────────────────────────────

test("per-session YOLO is sent to backend with sessionID", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-backend-1", Title: "Backend YOLO", YoloEnabled: false })],
  });

  await page.getByText("Backend YOLO").first().click();
  await page.locator("header").getByText("Yolo").click();

  const cmd = await waitForWSSend(page, "set_yolo");
  expect(cmd.payload).toHaveProperty("sessionID", "yolo-backend-1");
  expect((cmd.payload as { sessionID: string; enabled: boolean }).enabled).toBe(true);
});

test("per-session YOLO is restored when switching sessions", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-restore-a", Title: "Session A (YOLO)", YoloEnabled: true }),
      makeSession({ ID: "yolo-restore-b", Title: "Session B (no YOLO)", YoloEnabled: false }),
    ],
  });

  // Switch to Session A - should restore YOLO from backend YoloEnabled field
  await page.getByText("Session A (YOLO)").first().click();
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Switch to Session B - should restore OFF state from backend
  await page.getByText("Session B (no YOLO)").first().click();
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

test("backend YoloEnabled field is source of truth", async ({ page }) => {
  await page.goto("/");

  // Backend says YOLO is ON for this session
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-override", Title: "Override Test", YoloEnabled: true })],
  });

  await page.getByText("Override Test").first().click();

  // Should show lightning icon from backend field
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });
});

// ── Permission Rules Persistence ────────────────────────────────────────────

test("permission rules are fetched when modal is opened", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-rules-fetch", Title: "Rules Session", YoloEnabled: false })],
  });

  await page.getByText("Rules Session").first().click();

  // Open permissions modal
  await page.getByRole("button", { name: "Permissions" }).click();

  // Should send list_session_permissions command
  const cmd = await waitForWSSend(page, "list_session_permissions");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("perm-rules-fetch");
});

test("fetched permission rules are displayed in modal", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-display", Title: "Display Rules", YoloEnabled: false })],
  });

  await page.getByText("Display Rules").first().click();
  await page.getByRole("button", { name: "Permissions" }).click();

  // Send permission rules
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      {
        id: "rule-1",
        session_id: "perm-display",
        tool_name: "bash",
        action: "execute",
        path: "/tmp",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
      {
        id: "rule-2",
        session_id: "perm-display",
        tool_name: "write_file",
        action: "write",
        path: "/home/user/file.txt",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("write_file")).toBeVisible();
});

test("toggling permission rule sends update to backend", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-toggle-save", Title: "Toggle Save", YoloEnabled: false })],
  });

  await page.getByText("Toggle Save").first().click();
  await page.getByRole("button", { name: "Permissions" }).click();

  // Send initial rules
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      {
        id: "rule-toggle-1",
        session_id: "perm-toggle-save",
        tool_name: "bash",
        action: "execute",
        path: "/tmp",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });

  // Toggle rule off
  const checkbox = page.locator("button").filter({ hasText: "bash" }).locator("button").first();
  await checkbox.click();

  // Should send update_permission_rule
  const cmd = await waitForWSSend(page, "update_permission_rule");
  expect((cmd.payload as { ruleID: string; enabled: boolean }).ruleID).toBe("rule-toggle-1");
  expect((cmd.payload as { enabled: boolean }).enabled).toBe(false);
});

test("deleting permission rule sends delete to backend", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-delete-save", Title: "Delete Save", YoloEnabled: false })],
  });

  await page.getByText("Delete Save").first().click();
  await page.getByRole("button", { name: "Permissions" }).click();

  // Send rules
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      {
        id: "rule-delete-1",
        session_id: "perm-delete-save",
        tool_name: "bash",
        action: "execute",
        path: "/tmp",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });

  // Delete rule
  const deleteBtn = page.locator("button").filter({ has: page.locator("svg") }).locator("button").last();
  await deleteBtn.click();

  // Should send delete_permission_rule
  const cmd = await waitForWSSend(page, "delete_permission_rule");
  expect((cmd.payload as { ruleID: string }).ruleID).toBe("rule-delete-1");
});

// ── Cross-Session Persistence ────────────────────────────────────────────────

test("permission rules are session-specific", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "perm-cross-a", Title: "Session A", YoloEnabled: false }),
      makeSession({ ID: "perm-cross-b", Title: "Session B", YoloEnabled: false }),
    ],
  });

  // Open modal for Session A
  await page.getByText("Session A").first().click();
  await page.getByRole("button", { name: "Permissions" }).click();

  // Send rules for Session A
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      {
        id: "rule-a-1",
        session_id: "perm-cross-a",
        tool_name: "bash",
        action: "execute",
        path: "/tmp/a",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Close" }).click();

  // Switch to Session B and open modal
  await page.getByText("Session B").first().click();
  await page.getByRole("button", { name: "Permissions" }).click();

  // Should fetch rules for Session B (different)
  const cmd = await waitForWSSend(page, "list_session_permissions");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("perm-cross-b");

  // Send rules for Session B (empty)
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [],
  });

  // Should show empty state (no rules from Session A)
  await expect(page.getByText("No permission rules set for this session")).toBeVisible({ timeout: 2000 });
});

// ── Page Reload Persistence ───────────────────────────────────────────────────

test("per-session YOLO survives page reload (from backend)", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-reload-persist", Title: "Reload Persist", YoloEnabled: false })],
  });

  await page.getByText("Reload Persist").first().click();

  // Enable YOLO
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");

  // Simulate backend update with new YoloEnabled state
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "yolo-reload-persist", Title: "Reload Persist", YoloEnabled: true }),
  });

  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Reload page
  await setupMockWS(page);
  await page.reload();
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );

  // Re-send sessions with YOLO enabled (from backend)
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-reload-persist", Title: "Reload Persist", YoloEnabled: true })],
  });

  // Click on session again
  await page.getByText("Reload Persist").first().click();

  // YOLO should be restored from backend
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 3000 });
});

test("permission rules persist across page reload", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-reload-persist", Title: "Reload Rules", YoloEnabled: false })],
  });

  await page.getByText("Reload Rules").first().click();

  // Open modal and create rule (by sending permission grant)
  await page.getByRole("button", { name: "Permissions" }).click();
  await waitForWSSend(page, "list_session_permissions");

  // Simulate that a persistent permission was granted
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      {
        id: "rule-reload-1",
        session_id: "perm-reload-persist",
        tool_name: "read_file",
        action: "read",
        path: "/etc/passwd",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
    ],
  });

  await expect(page.getByText("read_file")).toBeVisible({ timeout: 2000 });

  // Close modal
  await page.getByRole("button", { name: "Close" }).click();

  // Reload page
  await setupMockWS(page);
  await page.reload();
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );

  // Re-send sessions
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "perm-reload-persist", Title: "Reload Rules", YoloEnabled: false })],
  });

  await page.getByText("Reload Rules").first().click();

  // Open modal again
  await page.getByRole("button", { name: "Permissions" }).click();
  await waitForWSSend(page, "list_session_permissions");

  // Send rules again (simulating backend persistence)
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      {
        id: "rule-reload-1",
        session_id: "perm-reload-persist",
        tool_name: "read_file",
        action: "read",
        path: "/etc/passwd",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
    ],
  });

  // Rule should still be there
  await expect(page.getByText("read_file")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("/etc/passwd")).toBeVisible();
});

// ── Settings Sync Between Multiple Clients ───────────────────────────────────

test("YOLO setting from one client doesn't affect other clients' sessions", async ({ page }) => {
  // This test verifies that settings are truly per-session and per-client

  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-multi-a", Title: "Multi A", YoloEnabled: false }),
      makeSession({ ID: "yolo-multi-b", Title: "Multi B", YoloEnabled: true }),
    ],
  });

  // Session B has YOLO enabled from backend
  await page.getByText("Multi B").first().click();
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Disable YOLO in Session B
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");

  // Simulate backend update
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "yolo-multi-b", Title: "Multi B", YoloEnabled: false }),
  });

  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });

  // Session A should still have its own state
  await page.getByText("Multi A").first().click();
  // Session A doesn't have YOLO enabled from backend
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

test("permission rules for one session don't affect another session", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "perm-sep-a", Title: "Sep A", YoloEnabled: false }),
      makeSession({ ID: "perm-sep-b", Title: "Sep B", YoloEnabled: false }),
    ],
  });

  // Add rule to Session A
  await page.getByText("Sep A").first().click();
  await page.getByRole("button", { name: "Permissions" }).click();
  await waitForWSSend(page, "list_session_permissions");

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      {
        id: "rule-sep-a",
        session_id: "perm-sep-a",
        tool_name: "bash",
        action: "execute",
        path: "/tmp/a",
        created_at: Math.floor(Date.now() / 1000),
        enabled: 1,
      },
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Close" }).click();

  // Switch to Session B
  await page.getByText("Sep B").first().click();
  await page.getByRole("button", { name: "Permissions" }).click();
  await waitForWSSend(page, "list_session_permissions");

  // Session B has no rules
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [],
  });

  await expect(page.getByText("No permission rules set for this session")).toBeVisible({ timeout: 2000 });
});
