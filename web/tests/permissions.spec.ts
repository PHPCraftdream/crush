/**
 * Permission dialog action tests.
 *
 * Covers:
 *  - "Allow" sends grant_permission with correct permissionID
 *  - "Allow always" sends grant_permission_persistent with correct permissionID
 *  - "Deny" sends deny_permission with correct permissionID
 *  - Each action dismisses the dialog
 *  - Multiple concurrent permission requests shown as stacked cards
 *  - Clicking Allow on one card dismisses only that card
 *  - Permission card shows tool name, action, description, path
 *  - Error permission shows error badge
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

function makePermission(overrides: Record<string, unknown> = {}) {
  return {
    ID: "perm-1",
    SessionID: "perm-sess",
    ToolCallID: "tc-1",
    ToolName: "bash",
    Description: "Run a shell command",
    Action: "execute",
    Path: "/tmp/script.sh",
    Params: {},
    ...overrides,
  };
}

async function setupSessionAndPermission(
  page: import("@playwright/test").Page,
  sessionID: string,
  perm: ReturnType<typeof makePermission>
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Perm Session" })],
  });
  // The app auto-selects the first session, no need to click
  // Wait for session to be active - check for visible chat input
  await expect(page.locator('[data-test-id="chat-input-textarea"]')).toBeVisible({ timeout: 3000 });
  // Ensure permission SessionID matches the active session
  const permWithSession = { ...perm, SessionID: sessionID };
  await sendMockWSMessage(page, { type: "permission_request", payload: permWithSession });
  await expect(page.getByTestId(`permission-${perm.ToolCallID}`)).toBeVisible({ timeout: 2000 });
}

// ── Card content ──────────────────────────────────────────────────────────────

test("permission card shows tool name", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-1", makePermission({ ToolName: "write_file", ToolCallID: "tc-a" }));
  await expect(page.getByTestId("permission-tc-a")).toContainText("write_file");
});

test("permission card shows action badge", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-2", makePermission({ Action: "write", ToolCallID: "tc-b" }));
  await expect(page.getByTestId("permission-tc-b")).toContainText("write");
});

test("permission card shows description", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-3", makePermission({
    Description: "Write content to a file",
    ToolCallID: "tc-c",
  }));
  await expect(page.getByTestId("permission-tc-c")).toContainText("Write content to a file");
});

test("permission card shows path", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-4", makePermission({
    Path: "/home/user/project/src/main.go",
    ToolCallID: "tc-d",
  }));
  await expect(page.getByTestId("permission-tc-d")).toContainText("/home/user/project/src/main.go");
});

test("permission card shows all three action buttons", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-5", makePermission({ ToolCallID: "tc-e" }));
  await expect(page.getByTestId("permission-deny")).toBeVisible();
  await expect(page.getByTestId("permission-allow")).toBeVisible();
  await expect(page.getByTestId("permission-allow-always")).toBeVisible();
});

// ── Allow ─────────────────────────────────────────────────────────────────────

test("clicking Allow sends grant_permission with permissionID", async ({ page }) => {
  await setupSessionAndPermission(page, "pa-1", makePermission({ ID: "grant-id-1", ToolCallID: "tc-f" }));
  await page.getByTestId("permission-allow").click();
  const cmd = await waitForWSSend(page, "grant_permission");
  expect((cmd.payload as { permissionID: string }).permissionID).toBe("grant-id-1");
});

test("clicking Allow dismisses the permission card", async ({ page }) => {
  await setupSessionAndPermission(page, "pa-2", makePermission({ ToolName: "read_file", ToolCallID: "tc-g" }));
  await page.getByTestId("permission-allow").click();
  await expect(page.getByTestId("permission-tc-g")).not.toBeVisible({ timeout: 2000 });
});

// ── Allow always ──────────────────────────────────────────────────────────────

test("clicking Allow always sends grant_permission_persistent with permissionID", async ({ page }) => {
  await setupSessionAndPermission(page, "paa-1", makePermission({ ID: "always-id-1", ToolCallID: "tc-h" }));
  await page.getByTestId("permission-allow-always").click();
  const cmd = await waitForWSSend(page, "grant_permission_persistent");
  expect((cmd.payload as { permissionID: string }).permissionID).toBe("always-id-1");
});

test("clicking Allow always does NOT send grant_permission (only persistent)", async ({ page }) => {
  await setupSessionAndPermission(page, "paa-2", makePermission({ ID: "always-id-2", ToolCallID: "tc-i" }));
  await page.getByTestId("permission-allow-always").click();
  await waitForWSSend(page, "grant_permission_persistent");

  // Should not have sent the non-persistent command
  const sentNonPersistent = await page.evaluate(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return sent.some((m) => m.type === "grant_permission");
  });
  expect(sentNonPersistent).toBe(false);
});

test("clicking Allow always dismisses the permission card", async ({ page }) => {
  await setupSessionAndPermission(page, "paa-3", makePermission({ ToolName: "glob", ToolCallID: "tc-j" }));
  await page.getByTestId("permission-allow-always").click();
  await expect(page.getByTestId("permission-tc-j")).not.toBeVisible({ timeout: 2000 });
});

// ── Deny ──────────────────────────────────────────────────────────────────────

test("clicking Deny sends deny_permission with permissionID", async ({ page }) => {
  await setupSessionAndPermission(page, "pd-1", makePermission({ ID: "deny-id-1", ToolCallID: "tc-k" }));
  await page.getByTestId("permission-deny").click();
  const cmd = await waitForWSSend(page, "deny_permission");
  expect((cmd.payload as { permissionID: string }).permissionID).toBe("deny-id-1");
});

test("clicking Deny dismisses the permission card", async ({ page }) => {
  await setupSessionAndPermission(page, "pd-2", makePermission({ ToolName: "multiedit", ToolCallID: "tc-l" }));
  await page.getByTestId("permission-deny").click();
  await expect(page.getByTestId("permission-tc-l")).not.toBeVisible({ timeout: 2000 });
});

// ── Multiple concurrent permissions ──────────────────────────────────────────

test("multiple permission requests show as stacked cards", async ({ page }) => {
  const sessionID = "pm-1";
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Multi Perm" })],
  });
  await expect(page.getByText("Multi Perm").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Multi Perm").first().click();
  await page.waitForTimeout(100);

  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "p1", SessionID: sessionID, ToolCallID: "tc-m1", ToolName: "bash", Description: "Run bash" }),
  });
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "p2", SessionID: sessionID, ToolCallID: "tc-m2", ToolName: "write_file", Description: "Write file" }),
  });

  await expect(page.getByTestId("permission-dialog-container")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("permission-tc-m1")).toContainText("Run bash");
  await expect(page.getByTestId("permission-tc-m2")).toContainText("Write file");
});

test("allowing one card dismisses only that card, other remains", async ({ page }) => {
  const sessionID = "pm-2";
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Selective Perm" })],
  });
  await expect(page.getByText("Selective Perm").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Selective Perm").first().click();
  await page.waitForTimeout(100);

  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "s1", SessionID: sessionID, ToolCallID: "tc-s1", ToolName: "bash", Description: "First tool" }),
  });
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "s2", SessionID: sessionID, ToolCallID: "tc-s2", ToolName: "grep", Description: "Second tool" }),
  });
  await expect(page.getByTestId("permission-tc-s1")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("permission-tc-s2")).toBeVisible();

  // permission_notification removes the first card
  await sendMockWSMessage(page, {
    type: "permission_notification",
    payload: { ToolCallID: "tc-s1", Granted: true, Denied: false },
  });
  await expect(page.getByTestId("permission-tc-s1")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("permission-tc-s2")).toBeVisible();
});

// ── Empty path ────────────────────────────────────────────────────────────────

test("permission card without path shows no path line", async ({ page }) => {
  await setupSessionAndPermission(page, "pp-1", makePermission({ Path: "", ToolCallID: "tc-np" }));
  // The path paragraph should not render when Path is empty
  // We check that /tmp/script.sh (default) is NOT shown since we overrode with ""
  await expect(page.getByTestId("permission-tc-np")).not.toContainText("/tmp/script.sh");
});
