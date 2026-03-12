/**
 * Debug test to understand YOLO sessionID issue
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  await page.addInitScript(() => {
    localStorage.removeItem("crush_yolo");
    localStorage.removeItem("crush_yolo_sessions");
  });
});

test("debug: check set_yolo payload when toggling YOLO", async ({ page }) => {
  await page.goto("/");

  // Send sessions list
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "debug-sess", Title: "Debug Session" })],
  });

  // Click on session
  await page.getByText("Debug Session").first().click();
  await page.waitForTimeout(500);

  // Check if YOLO button is visible (means session is active)
  const yoloButton = page.locator(".btn-toolbar").filter({ hasText: "Yolo" });
  await expect(yoloButton).toBeVisible({ timeout: 2000 });

  // Open permissions modal
  await yoloButton.click();
  await expect(page.getByText("YOLO Mode")).toBeVisible({ timeout: 2000 });

  // Clear any previous messages
  await page.evaluate(() => {
    ((window as unknown) as Record<string, unknown>)["__wsSent"] = [];
  });

  // Click YOLO toggle
  const toggle = page.locator(".relative.w-14.h-7").first();
  await toggle.click();

  // Wait for set_yolo message
  await page.waitForTimeout(200);

  // Check what was sent
  const sent = await page.evaluate(() => {
    return ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: unknown;
    }>;
  });

  console.log("Sent messages:", JSON.stringify(sent, null, 2));

  const setYoloMsg = sent.find((m) => m.type === "set_yolo");
  expect(setYoloMsg).toBeDefined();

  console.log("set_yolo payload:", JSON.stringify(setYoloMsg?.payload, null, 2));
});
