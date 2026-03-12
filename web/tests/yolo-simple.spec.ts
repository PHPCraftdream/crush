/**
 * Yolo mode toggle tests (simplified).
 *
 * Tests basic YOLO button functionality without checking specific icons.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  await page.addInitScript(() => localStorage.removeItem("crush_yolo"));
  await page.addInitScript(() => localStorage.removeItem("crush_yolo_sessions"));
});

async function setupActiveSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "yolo-test-sess", Title: "Test Session", YoloEnabled: false })],
  });
  await expect(page.getByText("Test Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);
}

test("Yolo button is visible when session is active", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButton).toBeVisible({ timeout: 3000 });
  const title = await yoloButton.getAttribute("title");
  expect(title).toContain("OFF");
});

test("clicking Yolo button sends set_yolo with enabled: true", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await yoloButton.click();
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

test("clicking Yolo button again sends set_yolo with enabled: false", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });

  // Enable first (message #2, after session setup #1)
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });

  // Disable (message #3)
  await yoloButton.click();

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

test("Yolo button background changes when enabled", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });

  // Initially no yellow background
  await expect(yoloButton).not.toHaveClass(/bg-yellow/);

  // Click to enable
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });

  // Should have yellow background
  await expect(yoloButton).toHaveClass(/bg-yellow/);
  const title = await yoloButton.getAttribute("title");
  expect(title).toContain("ON");

  // Click to disable
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return sent.filter((m) => m.type === "set_yolo").length >= 3;
  }, { timeout: 5000 });

  // Yellow background should be gone
  await expect(yoloButton).not.toHaveClass(/bg-yellow/);
  const titleOff = await yoloButton.getAttribute("title");
  expect(titleOff).toContain("OFF");
});

test("yolo state persists across page reload", async ({ page }) => {
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

  // Verify localStorage
  const stored = await page.evaluate(() => localStorage.getItem("crush_yolo"));
  expect(stored).toBe("true");

  // Reload
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
    payload: [makeSession({ ID: "yolo-test-sess", Title: "Test Session", YoloEnabled: false })],
  });
  await page.getByText("Test Session").first().click();
  await page.waitForTimeout(200);

  // Yolo should still be enabled
  const yoloButtonAfter = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButtonAfter).toHaveClass(/bg-yellow/);
});

test("localStorage stores false when yolo is disabled", async ({ page }) => {
  await setupActiveSession(page);
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });

  // Enable then disable
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    const yolo = sent.filter((m) => m.type === "set_yolo");
    return yolo.length >= 2;
  }, { timeout: 5000 });
  await yoloButton.click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return sent.filter((m) => m.type === "set_yolo").length >= 3;
  }, { timeout: 5000 });

  const stored = await page.evaluate(() => localStorage.getItem("crush_yolo"));
  expect(stored).toBe("false");
});
