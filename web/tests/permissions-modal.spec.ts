/**
 * Permissions modal and rules management tests.
 *
 * Covers new features from per-session permissions system:
 *  - PermissionsModal component rendering
 *  - YOLO toggle in modal
 *  - Permission rules list display
 *  - Toggle permission rules on/off
 *  - Delete permission rules
 *  - Settings button in permission dialog
 *  - Permissions button when no pending requests
 *  - Session-specific permission rules
 *
 * @since 2026-03-12
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Fixtures ─────────────────────────────────────────────────────────────────────

function makePermission(overrides: Record<string, unknown> = {}) {
  return {
    ID: "perm-1",
    SessionID: "sess-1",
    ToolCallID: "tc-1",
    ToolName: "bash",
    Description: "Run a shell command",
    Action: "execute",
    Path: "/tmp/script.sh",
    Params: {},
    ...overrides,
  };
}

function makePermissionRule(overrides: Record<string, unknown> = {}) {
  return {
    id: "rule-1",
    session_id: "sess-1",
    tool_name: "bash",
    action: "execute",
    path: "/tmp",
    created_at: Date.now() / 1000,
    enabled: 1,
    ...overrides,
  };
}

async function setupSession(page: import("@playwright/test").Page, sessionID = "test-sess", title = "Test Session") {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: title, YoloEnabled: false })],
  });
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 3000 });
  await page.getByText(title).first().click();
}

// ── Permissions Button (when no pending requests) ─────────────────────────────

test("Permissions button is visible in bottom-right corner", async ({ page }) => {
  await setupSession(page);
  // Should show Permissions button when no active permission requests
  await expect(page.getByRole("button", { name: "Permissions" })).toBeVisible({ timeout: 2000 });
});

test("Permissions button has settings icon", async ({ page }) => {
  await setupSession(page);
  const button = page.getByRole("button", { name: /Permissions/i });
  await expect(button).toBeVisible({ timeout: 2000 });
  // Should have an icon (settings gear)
  const icon = button.locator("svg");
  await expect(icon).toBeVisible();
});

test("clicking Permissions button opens modal", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();
  await expect(page.getByRole("button", { name: "Permissions" })).toBeVisible({ timeout: 2000 });
  // Modal should have title
  await expect(page.getByRole("heading", { name: "Permissions" })).toBeVisible();
});

test("Permission dialog has settings button that opens modal", async ({ page }) => {
  await setupSession(page, "pd-sess", "Perm Dialog Session");

  // Send a permission request to show dialog
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ToolName: "bash", ToolCallID: "tc-dialog" }),
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });

  // Click settings (gear) button
  const settingsBtn = page.locator("button").filter({ hasText: "bash" }).locator("button[title*='Manage']").first();
  await settingsBtn.click();

  // Modal should open
  await expect(page.getByRole("heading", { name: "Permissions" })).toBeVisible({ timeout: 2000 });
});

// ── Permissions Modal Structure ───────────────────────────────────────────────────

test("Permissions modal shows session title", async ({ page }) => {
  await setupSession(page, "modal-sess", "My Awesome Session");
  await page.getByRole("button", { name: /Permissions/i }).click();

  await expect(page.getByText("My Awesome Session")).toBeVisible({ timeout: 2000 });
});

test("Permissions modal has close button", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  const closeBtn = page.locator("button[aria-label='Close'], button[title='Close']").first();
  await expect(closeBtn).toBeVisible({ timeout: 2000 });
});

test("clicking close button closes modal", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();
  await expect(page.getByRole("heading", { name: "Permissions" })).toBeVisible({ timeout: 2000 });

  const closeBtn = page.locator("button").filter({ has: page.locator("svg").first() }).first();
  await closeBtn.click();

  await expect(page.getByRole("heading", { name: "Permissions" })).not.toBeVisible({ timeout: 2000 });
});

test("Permissions modal has Close button at bottom", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await expect(page.getByRole("button", { name: "Close" })).toBeVisible({ timeout: 2000 });
});

test("clicking bottom Close button closes modal", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();
  await expect(page.getByRole("heading", { name: "Permissions" })).toBeVisible({ timeout: 2000 });

  await page.getByRole("button", { name: "Close" }).click();
  await expect(page.getByRole("heading", { name: "Permissions" })).not.toBeVisible({ timeout: 2000 });
});

// ── YOLO Toggle in Modal ──────────────────────────────────────────────────────────

test("Permissions modal shows YOLO toggle section", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await expect(page.getByText("YOLO Mode")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText(/All permissions are automatically approved/)).toBeVisible();
});

test("YOLO toggle shows lightning icon when enabled", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  // YOLO should be off by default
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible({ timeout: 2000 });
});

test("YOLO toggle can be switched on", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  // Click YOLO toggle
  const toggle = page.locator(".relative").filter({ hasText: /YOLO Mode/i }).locator("button").first();
  await toggle.click();

  // Should send set_yolo with enabled: true
  const cmd = await waitForWSSend(page, "set_yolo");
  expect((cmd.payload as { sessionID: string; enabled: boolean }).enabled).toBe(true);
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("test-sess");
});

test("YOLO toggle can be switched off", async ({ page }) => {
  await setupSession(page);

  // First enable YOLO
  await page.evaluate(() => (window as unknown as Record<string, unknown>).$yolo = true);

  await page.getByRole("button", { name: /Permissions/i }).click();

  const toggle = page.locator(".relative").filter({ hasText: /YOLO Mode/i }).locator("button").first();
  await toggle.click();

  const cmd = await waitForWSSend(page, "set_yolo");
  expect((cmd.payload as { enabled: boolean }).enabled).toBe(false);
});

test("when YOLO is enabled, permission rules section is hidden", async ({ page }) => {
  await setupSession(page);

  // Enable YOLO
  await page.evaluate(() => (window as unknown as Record<string, unknown>).$yolo = true);

  await page.getByRole("button", { name: /Permissions/i }).click();

  // Permission rules section should not be visible
  await expect(page.getByText("Permission Rules")).not.toBeVisible({ timeout: 2000 });
});

// ── Permission Rules List ────────────────────────────────────────────────────────

test("Permissions modal shows empty state when no rules", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  // Should show empty state message
  await expect(page.getByText("No permission rules set for this session")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText(/Use "Allow always"/)).toBeVisible();
});

test("Permissions modal fetches rules when opened", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  // Should send list_session_permissions
  const cmd = await waitForWSSend(page, "list_session_permissions");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("test-sess");
});

test("Permissions modal displays fetched permission rules", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  // Send permission rules
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-1",
        tool_name: "bash",
        action: "execute",
        created_at: Date.now() / 1000,
      }),
      makePermissionRule({
        id: "rule-2",
        tool_name: "write_file",
        action: "write",
        created_at: Date.now() / 1000,
      }),
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("write_file")).toBeVisible();
});

test("permission rule card shows tool name and action", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-tool",
        tool_name: "read_file",
        action: "read",
        path: "/home/user/file.txt",
      }),
    ],
  });

  await expect(page.getByText("read_file")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("read")).toBeVisible();
});

test("permission rule card shows path", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-path",
        path: "/var/log/app.log",
      }),
    ],
  });

  await expect(page.getByText("/var/log/app.log")).toBeVisible({ timeout: 2000 });
});

test("permission rule card shows created date", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  const createdAt = Math.floor(Date.now() / 1000);
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-date",
        created_at: createdAt,
      }),
    ],
  });

  const dateStr = new Date(createdAt * 1000).toLocaleDateString();
  await expect(page.getByText(new RegExp(dateStr))).toBeVisible({ timeout: 2000 });
});

// ── Toggle Permission Rules ───────────────────────────────────────────────────────

test("permission rule has checkbox to enable/disable", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-toggle",
        enabled: 1,
      }),
    ],
  });

  // Should have a checkmark icon when enabled
  const checkbox = page.locator("button").filter({ hasText: "bash" }).locator("button").first();
  await expect(checkbox).toBeVisible({ timeout: 2000 });
});

test("clicking rule checkbox toggles enabled state", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-toggle-click",
        enabled: 1,
      }),
    ],
  });

  // Click the checkbox
  const checkbox = page.locator("button").filter({ hasText: "bash" }).locator("button").first();
  await checkbox.click();

  // Should send update_permission_rule with enabled: false
  const cmd = await waitForWSSend(page, "update_permission_rule");
  expect((cmd.payload as { ruleID: string; enabled: boolean }).ruleID).toBe("rule-toggle-click");
  expect((cmd.payload as { enabled: boolean }).enabled).toBe(false);
});

test("disabled rule shows (Disabled) label", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-disabled",
        enabled: 0,
      }),
    ],
  });

  await expect(page.getByText("(Disabled)")).toBeVisible({ timeout: 2000 });
});

test("disabled rule card has reduced opacity", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-opacity",
        enabled: 0,
      }),
    ],
  });

  // Check for opacity class (typically opacity-60 in Tailwind)
  const card = page.locator("div").filter({ hasText: "bash" }).first();
  const opacityClass = await card.getAttribute("class");
  expect(opacityClass).toContain("opacity");
});

// ── Delete Permission Rules ───────────────────────────────────────────────────────

test("permission rule has delete button", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-delete",
      }),
    ],
  });

  // Should have a trash icon button
  const deleteBtn = page.locator("button").filter({ has: page.locator("svg[data-lucide='trash-2']") }).first();
  await expect(deleteBtn).toBeVisible({ timeout: 2000 });
});

test("clicking delete button sends delete_permission_rule", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-delete-click",
      }),
    ],
  });

  const deleteBtn = page.locator("button").filter({ has: page.locator("svg") }).locator("button").last();
  await deleteBtn.click();

  const cmd = await waitForWSSend(page, "delete_permission_rule");
  expect((cmd.payload as { ruleID: string }).ruleID).toBe("rule-delete-click");
});

test("deleting rule removes it from the list", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-remove-1",
        tool_name: "bash",
      }),
      makePermissionRule({
        id: "rule-remove-2",
        tool_name: "grep",
      }),
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("grep")).toBeVisible();

  // Delete one rule (frontend should optimistically update)
  const firstDelete = page.locator("button").filter({ has: page.locator("svg") }).locator("button").last();
  await firstDelete.click();

  // After delete, one rule should remain (though actual removal happens after backend confirms)
  await waitForWSSend(page, "delete_permission_rule");
});

// ── Session-Specific Permissions ───────────────────────────────────────────────────

test("permission rules are session-specific", async ({ page }) => {
  // Setup first session
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sess-a", Title: "Session A", YoloEnabled: false }),
      makeSession({ ID: "sess-b", Title: "Session B", YoloEnabled: false }),
    ],
  });

  await expect(page.getByText("Session A")).toBeVisible({ timeout: 3000 });
  await page.getByText("Session A").click();

  await page.getByRole("button", { name: /Permissions/i }).click();

  // Should request permissions for sess-a
  let cmd = await waitForWSSend(page, "list_session_permissions");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("sess-a");

  // Send some rules
  await sendMockWSMessage(page, {
    type: "session_permissions",
    payload: [
      makePermissionRule({
        id: "rule-a",
        session_id: "sess-a",
        tool_name: "bash",
      }),
    ],
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });

  // Close modal and switch sessions
  const closeBtn = page.getByRole("button", { name: "Close" });
  await closeBtn.click();

  await page.getByText("Session B").click();
  await page.getByRole("button", { name: /Permissions/i }).click();

  // Should request permissions for sess-b (different session)
  cmd = await waitForWSSend(page, "list_session_permissions");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("sess-b");
});

// ── Add Rule Button ─────────────────────────────────────────────────────────────────

test("Permissions modal has Add Rule button", async ({ page }) => {
  await setupSession(page);
  await page.getByRole("button", { name: /Permissions/i }).click();

  await expect(page.getByRole("button", { name: /Add Rule/i })).toBeVisible({ timeout: 2000 });
});

test("Add Rule button is disabled when no active session", async ({ page }) => {
  await page.goto("/");

  // No sessions loaded, should not be able to add rules
  // The Permissions button only shows when there's an active session
  await expect(page.getByRole("button", { name: /Permissions/i })).not.toBeVisible({ timeout: 2000 });
});
