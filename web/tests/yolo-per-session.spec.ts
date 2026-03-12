/**
 * Per-session YOLO mode tests (append to existing yolo.spec.ts).
 *
 * Additional tests for session-specific YOLO feature (commit 6a39c56e):
 *  - set_yolo sends sessionID with active session
 *  - Different sessions can have different YOLO states
 *  - YOLO state persists per session
 *  - Session YoloEnabled field from backend
 *  - Per-session YOLO in localStorage
 *
 * @since 2026-03-12
 * Add this to the existing web/tests/yolo.spec.ts file
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  // Clear all localStorage for clean state
  await page.addInitScript(() => {
    localStorage.removeItem("crush_yolo");
    localStorage.removeItem("crush_yolo_sessions");
  });
});

// ── Per-Session YOLO State ──────────────────────────────────────────────────────────

test("set_yolo command includes sessionID when session is active", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-sess-1", Title: "Yolo Session 1", YoloEnabled: false })],
  });
  await expect(page.getByText("Yolo Session 1")).toBeVisible({ timeout: 3000 });
  await page.getByText("Yolo Session 1").click();

  // Enable YOLO
  await page.locator("header").getByText("Yolo").click();
  const cmd = await waitForWSSend(page, "set_yolo");

  expect(cmd.payload).toHaveProperty("sessionID", "yolo-sess-1");
  expect((cmd.payload as { sessionID: string; enabled: boolean }).enabled).toBe(true);
});

test("different sessions can have different YOLO states", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-sess-a", Title: "Session A", YoloEnabled: false }),
      makeSession({ ID: "yolo-sess-b", Title: "Session B", YoloEnabled: true }),
    ],
  });
  await expect(page.getByText("Session A")).toBeVisible({ timeout: 3000 });

  // Enable YOLO in Session A
  await page.getByText("Session A").click();
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Switch to Session B (which has YOLO enabled from backend)
  await page.getByText("Session B").click();

  // Session B should also show YOLO active (from its YoloEnabled state)
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });
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
  await page.getByText("Session A (YOLO OFF)").click();
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });

  // Switch to Session B (YOLO on)
  await page.getByText("Session B (YOLO ON)").click();
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Switch back to Session A
  await page.getByText("Session A (YOLO OFF)").click();
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

// ── Per-Session YOLO Persistence ───────────────────────────────────────────────────

test("YOLO state is stored per session in localStorage", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-persist-1", Title: "Persist Session 1", YoloEnabled: false }),
      makeSession({ ID: "yolo-persist-2", Title: "Persist Session 2", YoloEnabled: false }),
    ],
  });

  // Enable YOLO in Session 1
  await page.getByText("Persist Session 1").click();
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");

  // Check localStorage
  const yoloSessions = await page.evaluate(() => {
    const stored = localStorage.getItem("crush_yolo_sessions");
    return stored ? JSON.parse(stored) : {};
  });

  expect(yoloSessions).toHaveProperty("yolo-persist-1", true);
  expect(yoloSessions).not.toHaveProperty("yolo-persist-2");
});

test("switching sessions restores YOLO state from localStorage", async ({ page }) => {
  await page.goto("/");

  // Pre-populate localStorage with different YOLO states
  await page.addInitScript(() => {
    localStorage.setItem("crush_yolo_sessions", JSON.stringify({
      "yolo-restore-a": true,
      "yolo-restore-b": false,
    }));
  });

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-restore-a", Title: "Session A (should be YOLO)", YoloEnabled: false }),
      makeSession({ ID: "yolo-restore-b", Title: "Session B (should not)", YoloEnabled: false }),
    ],
  });

  // Switch to Session A - should restore YOLO ON
  await page.getByText("Session A (should be YOLO)").click();
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Switch to Session B - should restore YOLO OFF
  await page.getByText("Session B (should not)").click();
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

test("YOLO state persists across page reload per session", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-reload-1", Title: "Reload Session", YoloEnabled: false }),
    ],
  });

  // Enable YOLO
  await page.getByText("Reload Session").click();
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Verify localStorage
  const beforeReload = await page.evaluate(() => {
    const stored = localStorage.getItem("crush_yolo_sessions");
    return stored ? JSON.parse(stored) : {};
  });
  expect(beforeReload).toHaveProperty("yolo-reload-1", true);

  // Reload page
  await setupMockWS(page);
  await page.reload();
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );

  // Re-send sessions (simulating backend response)
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-reload-1", Title: "Reload Session", YoloEnabled: false }),
    ],
  });

  // Click on session again
  await page.getByText("Reload Session").click();

  // YOLO should still be ON (restored from localStorage)
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 3000 });
});

// ── Backend YoloEnabled Field ────────────────────────────────────────────────────────

test("session with YoloEnabled=true from backend shows lightning icon", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-backend-on", Title: "YOLO Session", YoloEnabled: true })],
  });

  await expect(page.getByText("YOLO Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("YOLO Session").click();

  // Should show lightning icon immediately
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });
});

test("session with YoloEnabled=false from backend shows lock icon", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-backend-off", Title: "No YOLO Session", YoloEnabled: false })],
  });

  await expect(page.getByText("No YOLO Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("No YOLO Session").click();

  // Should show lock icon
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

test("session_updated event with YoloEnabled change updates UI", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-update", Title: "Update YOLO", YoloEnabled: false })],
  });

  await page.getByText("Update YOLO").click();
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });

  // Backend updates YOLO state
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "yolo-update", Title: "Update YOLO", YoloEnabled: true }),
  });

  // UI should update to show lightning icon
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });
});

// ── Permission Auto-Grant with Per-Session YOLO ───────────────────────────────────

test("permission_request is auto-granted when session YOLO is enabled", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-perm", Title: "YOLO Perm Session", YoloEnabled: true })],
  });

  await page.getByText("YOLO Perm Session").click();

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

  await page.getByText("No YOLO Perm").click();

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

// ── Multiple Sessions with Different YOLO States ───────────────────────────────────

test("permission request in non-YOLO session while another has YOLO", async ({ page }) => {
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "yolo-multi-a", Title: "YOLO Session", YoloEnabled: true }),
      makeSession({ ID: "yolo-multi-b", Title: "Non-YOLO Session", YoloEnabled: false }),
    ],
  });

  // Active in non-YOLO session
  await page.getByText("Non-YOLO Session").click();

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
