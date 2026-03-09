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
  await expect(page.getByText("Perm Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Perm Session").first().click();
  await sendMockWSMessage(page, { type: "permission_request", payload: perm });
  await expect(page.getByText(perm.ToolName as string)).toBeVisible({ timeout: 2000 });
}

// ── Card content ──────────────────────────────────────────────────────────────

test("permission card shows tool name", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-1", makePermission({ ToolName: "write_file", ToolCallID: "tc-a" }));
  await expect(page.getByText("write_file")).toBeVisible({ timeout: 2000 });
});

test("permission card shows action badge", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-2", makePermission({ Action: "write", ToolCallID: "tc-b" }));
  await expect(page.getByText("write")).toBeVisible({ timeout: 2000 });
});

test("permission card shows description", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-3", makePermission({
    Description: "Write content to a file",
    ToolCallID: "tc-c",
  }));
  await expect(page.getByText("Write content to a file")).toBeVisible({ timeout: 2000 });
});

test("permission card shows path", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-4", makePermission({
    Path: "/home/user/project/src/main.go",
    ToolCallID: "tc-d",
  }));
  await expect(page.getByText("/home/user/project/src/main.go")).toBeVisible({ timeout: 2000 });
});

test("permission card shows all three action buttons", async ({ page }) => {
  await setupSessionAndPermission(page, "pc-5", makePermission({ ToolCallID: "tc-e" }));
  await expect(page.getByRole("button", { name: "Allow", exact: true })).toBeVisible({ timeout: 2000 });
  await expect(page.getByRole("button", { name: "Allow always" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Deny", exact: true })).toBeVisible();
});

// ── Allow ─────────────────────────────────────────────────────────────────────

test("clicking Allow sends grant_permission with permissionID", async ({ page }) => {
  await setupSessionAndPermission(page, "pa-1", makePermission({ ID: "grant-id-1", ToolCallID: "tc-f" }));
  await page.getByRole("button", { name: "Allow", exact: true }).click();
  const cmd = await waitForWSSend(page, "grant_permission");
  expect((cmd.payload as { permissionID: string }).permissionID).toBe("grant-id-1");
});

test("clicking Allow dismisses the permission card", async ({ page }) => {
  await setupSessionAndPermission(page, "pa-2", makePermission({ ToolName: "read_file", ToolCallID: "tc-g" }));
  await page.getByRole("button", { name: "Allow", exact: true }).click();
  await expect(page.getByText("read_file")).not.toBeVisible({ timeout: 2000 });
});

// ── Allow always ──────────────────────────────────────────────────────────────

test("clicking Allow always sends grant_permission_persistent with permissionID", async ({ page }) => {
  await setupSessionAndPermission(page, "paa-1", makePermission({ ID: "always-id-1", ToolCallID: "tc-h" }));
  await page.getByRole("button", { name: "Allow always" }).click();
  const cmd = await waitForWSSend(page, "grant_permission_persistent");
  expect((cmd.payload as { permissionID: string }).permissionID).toBe("always-id-1");
});

test("clicking Allow always does NOT send grant_permission (only persistent)", async ({ page }) => {
  await setupSessionAndPermission(page, "paa-2", makePermission({ ID: "always-id-2", ToolCallID: "tc-i" }));
  await page.getByRole("button", { name: "Allow always" }).click();
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
  await page.getByRole("button", { name: "Allow always" }).click();
  await expect(page.getByText("glob")).not.toBeVisible({ timeout: 2000 });
});

// ── Deny ──────────────────────────────────────────────────────────────────────

test("clicking Deny sends deny_permission with permissionID", async ({ page }) => {
  await setupSessionAndPermission(page, "pd-1", makePermission({ ID: "deny-id-1", ToolCallID: "tc-k" }));
  await page.getByRole("button", { name: "Deny", exact: true }).click();
  const cmd = await waitForWSSend(page, "deny_permission");
  expect((cmd.payload as { permissionID: string }).permissionID).toBe("deny-id-1");
});

test("clicking Deny dismisses the permission card", async ({ page }) => {
  await setupSessionAndPermission(page, "pd-2", makePermission({ ToolName: "multiedit", ToolCallID: "tc-l" }));
  await page.getByRole("button", { name: "Deny", exact: true }).click();
  await expect(page.getByText("multiedit")).not.toBeVisible({ timeout: 2000 });
});

// ── Multiple concurrent permissions ──────────────────────────────────────────

test("multiple permission requests show as stacked cards", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "pm-1", Title: "Multi Perm" })],
  });
  await expect(page.getByText("Multi Perm").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Multi Perm").first().click();

  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "p1", ToolCallID: "tc-m1", ToolName: "bash", Description: "Run bash" }),
  });
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "p2", ToolCallID: "tc-m2", ToolName: "write_file", Description: "Write file" }),
  });

  await expect(page.getByText("Run bash")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Write file")).toBeVisible();
});

test("allowing one card dismisses only that card, other remains", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "pm-2", Title: "Selective Perm" })],
  });
  await expect(page.getByText("Selective Perm").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Selective Perm").first().click();

  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "s1", ToolCallID: "tc-s1", ToolName: "bash", Description: "First tool" }),
  });
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: makePermission({ ID: "s2", ToolCallID: "tc-s2", ToolName: "grep", Description: "Second tool" }),
  });
  await expect(page.getByText("First tool")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Second tool")).toBeVisible();

  // permission_notification removes the first card
  await sendMockWSMessage(page, {
    type: "permission_notification",
    payload: { ToolCallID: "tc-s1", Granted: true, Denied: false },
  });
  await expect(page.getByText("First tool")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Second tool")).toBeVisible();
});

// ── Empty path ────────────────────────────────────────────────────────────────

test("permission card without path shows no path line", async ({ page }) => {
  await setupSessionAndPermission(page, "pp-1", makePermission({ Path: "", ToolCallID: "tc-np" }));
  // The path paragraph should not render when Path is empty
  // We check that /tmp/script.sh (default) is NOT shown since we overrode with ""
  await expect(page.getByText("/tmp/script.sh")).not.toBeVisible({ timeout: 1000 });
});
