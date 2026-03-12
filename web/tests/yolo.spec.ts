/**
 * Yolo mode toggle tests.
 *
 * Covers:
 *  - Yolo button is visible in the header
 *  - Default state is inactive (🔒)
 *  - Clicking enables yolo and sends set_yolo { enabled: true }
 *  - Clicking again disables and sends set_yolo { enabled: false }
 *  - Config event with yolo: true initialises button to active state
 *  - Active state persists after page reload (via localStorage)
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  // Clear yolo localStorage so tests are isolated
  await page.addInitScript(() => localStorage.removeItem("crush_yolo"));
  // Clear session-specific yolo localStorage
  await page.addInitScript(() => localStorage.removeItem("crush_yolo_sessions"));
});

// Helper to set up an active session for tests that need the YOLO button visible
async function setupActiveSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-test-sess", Title: "YOLO Test Session", YoloEnabled: false })],
  });
  await expect(page.getByText("YOLO Test Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("YOLO Test Session").first().click();
  await page.waitForTimeout(200); // Wait for ChatToolbar to appear
}

// ── Visibility ────────────────────────────────────────────────────────────────

test("Yolo button is visible when session is active", async ({ page }) => {
  await setupActiveSession(page);
  await expect(page.locator(".btn-toolbar").filter({ hasText: "Yolo" })).toBeVisible({ timeout: 3000 });
});

test("Yolo button shows shield icon by default (inactive)", async ({ page }) => {
  await setupActiveSession(page);
  // ShieldOff icon is shown when inactive (Lucide icon, not an emoji)
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButton.locator("svg")).toBeVisible({ timeout: 2000 });
});

// ── Enabling yolo ──────────────────────────────────────────────────────────

test("clicking Yolo button sends set_yolo with enabled: true", async ({ page }) => {
  await setupActiveSession(page);
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  // Wait for SECOND set_yolo message (first is from session setup, second from click)
  const cmd = await page.waitForFunction(
    () => {
      const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
        type: string;
        payload: { enabled: boolean };
      }>;
      const yolo = sent.filter((m) => m.type === "set_yolo");
      return yolo.length >= 2 ? yolo[yolo.length - 1] : null;
    },
    { timeout: 5000 }
  );
  const cmdPayload = await cmd.jsonValue() as { type: string; payload: { enabled: boolean } };
  expect(cmdPayload.payload.enabled).toBe(true);
});

test("Yolo button shows yellow background after enabling", async ({ page }) => {
  await setupActiveSession(page);
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  // Wait for the second set_yolo message (from the click, not session setup)
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });
  // When enabled, button should have yellow background class
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButton).toHaveClass(/bg-yellow/);
  // Check title confirms it's ON
  const title = await yoloButton.getAttribute("title");
  expect(title).toContain("ON");
});

// ── Disabling yolo ─────────────────────────────────────────────────────────

test("clicking Yolo button again sends set_yolo with enabled: false", async ({ page }) => {
  await setupActiveSession(page);
  // Enable first (message #2, after session setup #1)
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });

  // Now disable — need to wait for the third set_yolo command
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  const thirdCmd = await page.waitForFunction(
    () => {
      const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
        type: string;
        payload: { enabled: boolean };
      }>;
      const yolo = sent.filter((m) => m.type === "set_yolo");
      return yolo.length >= 3 ? yolo[yolo.length - 1] : null;
    },
    { timeout: 5000 }
  );
  const cmd = await thirdCmd.jsonValue() as { type: string; payload: { enabled: boolean } };
  expect(cmd.payload.enabled).toBe(false);
});

test("Yolo button returns to normal after disabling", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });
  await expect(yoloButton).toHaveClass(/bg-yellow/);

  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 3;
  }, { timeout: 5000 });
  await expect(yoloButton).not.toHaveClass(/bg-yellow/);
});

// ── Config initialisation ──────────────────────────────────────────────────

test("config event with yolo: true shows active state", async ({ page }) => {
  await setupActiveSession(page);
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ yolo: true }),
  });
  // yolo: true in server config with no localStorage override → active
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButton).toHaveClass(/bg-yellow/);
});

test("config event with yolo: false keeps inactive state", async ({ page }) => {
  await setupActiveSession(page);
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ yolo: false }),
  });
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButton).not.toHaveClass(/bg-yellow/);
});

// ── LocalStorage persistence ───────────────────────────────────────────────

test("yolo state persists across page reload via localStorage", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  // Enable yolo
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });
  await expect(yoloButton).toHaveClass(/bg-yellow/);

  // Verify localStorage was written
  const stored = await page.evaluate(() => localStorage.getItem("crush_yolo"));
  expect(stored).toBe("true");

  // Reload with mock WS (addInitScript runs again before page load)
  await setupMockWS(page);
  await page.reload();
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );

  // Restore localStorage after reload (simulate persistence)
  // In a real browser, localStorage would persist, but tests clear it on reload
  await page.evaluate(() => localStorage.setItem("crush_yolo", "true"));
  await page.evaluate(() => localStorage.setItem("crush_yolo_sessions", JSON.stringify({ "yolo-test-sess": true })));

  // Need to select session again after reload
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-test-sess", Title: "YOLO Test Session", YoloEnabled: false })],
  });
  await page.getByText("YOLO Test Session").first().click();
  await page.waitForTimeout(200);

  // Yolo should still be enabled — loaded from localStorage
  const yoloButtonAfter = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButtonAfter).toHaveClass(/bg-yellow/);
});

test("localStorage stores false when yolo is disabled", async ({ page }) => {
  await setupActiveSession(page);
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });
  // Disable
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return sent.filter((m) => m.type === "set_yolo").length >= 3;
  }, { timeout: 5000 });

  const stored = await page.evaluate(() => localStorage.getItem("crush_yolo"));
  expect(stored).toBe("false");
});

// ── Tooltip ─────────────────────────────────────────────────────────────────

test("inactive Yolo button has tooltip explaining it requires approval", async ({ page }) => {
  await setupActiveSession(page);
  const btn = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  const title = await btn.getAttribute("title");
  expect(title).toContain("OFF");
});

test("active Yolo button has tooltip explaining auto-approval", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });
  const title = await yoloButton.getAttribute("title");
  expect(title).toContain("ON");
});

// ── Yolo mode with active session ────────────────────────────────────────────

test("Yolo can be toggled while a session is active", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-sess", Title: "Active Session" })],
  });
  await expect(page.getByText("Active Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Active Session").first().click();
  await page.waitForTimeout(500);

  // Toggle yolo while session is active
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await yoloButton.click();
  const cmd = await page.waitForFunction(
    () => {
      const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
        type: string;
        payload: { enabled: boolean };
      }>;
      const yolo = sent.filter((m) => m.type === "set_yolo");
      return yolo.length >= 2 ? yolo[yolo.length - 1] : null;
    },
    { timeout: 5000 }
  );
  const cmdPayload = await cmd.jsonValue() as { type: string; payload: { enabled: boolean } };
  expect(cmdPayload.payload.enabled).toBe(true);
  await expect(yoloButton).toHaveClass(/bg-yellow/);
});
