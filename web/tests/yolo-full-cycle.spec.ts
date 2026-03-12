/**
 * YOLO Full Cycle TDD Test
 *
 * Complete test covering:
 * 1. Toggle YOLO ON
 * 2. Verify it saves to backend
 * 3. Switch away and back to session
 * 4. Verify YOLO state is restored
 * 5. Toggle YOLO OFF
 * 6. Verify it persists
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

test("YOLO full cycle: toggle ON, persist, restore, toggle OFF", async ({ page }) => {
  await page.goto("/");

  // Step 1: Create session and select it
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "cycle-sess", Title: "Cycle Test", YoloEnabled: false })],
  });
  await page.getByText("Cycle Test").first().click();
  await page.waitForTimeout(300);

  // Debug: Verify session became active (check for active styling)
  const activeSessionCheck = await page.evaluate(() => {
    const sessionEl = document.querySelector(".bg-canvas.border-accent\\/20");
    return {
      hasActiveStyling: !!sessionEl,
      sessionText: sessionEl?.textContent || null
    };
  });
  console.log("Active session check:", activeSessionCheck);

  // Debug: Check if ChatToolbar is rendered
  const toolbarCheck = await page.evaluate(() => {
    const toolbar = document.querySelector(".px-8.pt-3.pb-1");
    const yoloBtn = toolbar?.querySelector(".btn-toolbar");
    return {
      hasToolbar: !!toolbar,
      hasYoloBtn: !!yoloBtn,
      yoloBtnText: yoloBtn?.textContent || ""
    };
  });
  console.log("Toolbar check:", toolbarCheck);

  // Step 2: Open permissions modal and verify YOLO is OFF
  // Find YOLO button more precisely
  const yoloBtn = page.locator("button.btn-toolbar").filter({ hasText: /^Yolo$/ });
  await expect(yoloBtn).toBeVisible();
  await yoloBtn.click();

  await expect(page.getByText("YOLO Mode")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible();

  const toggle = page.locator(".relative.w-14.h-7").first();
  const knob = page.locator(".absolute.top-1");

  // Verify OFF state
  await expect(toggle).toHaveClass(/bg-surface/);
  await expect(knob).toHaveClass(/translate-x-1/);

  // Step 3: Toggle YOLO ON
  await toggle.click();
  await page.waitForTimeout(200);

  // Verify UI updates to ON state
  await expect(knob).toHaveClass(/translate-x-7/);
  await expect(toggle).toHaveClass(/bg-yellow/);
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible();

  // Step 4: Verify set_yolo was sent to backend
  const setYoloCmd = await waitForWSSend(page, "set_yolo");
  console.log("set_yolo payload:", JSON.stringify(setYoloCmd.payload));
  expect((setYoloCmd.payload as { sessionID: string; enabled: boolean }).sessionID).toBe("cycle-sess");
  expect((setYoloCmd.payload as { enabled: boolean }).enabled).toBe(true);

  // Step 5: Backend sends session_updated confirming the change
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "cycle-sess", Title: "Cycle Test", YoloEnabled: true }),
  });

  // Step 6: Close modal and verify toolbar button shows YOLO is ON
  await page.locator("button").filter({ hasText: "Close" }).click();

  const yoloToolbarBtn = page.locator("button.btn-toolbar").filter({ hasText: /^Yolo$/ });
  await expect(yoloToolbarBtn).toHaveClass(/bg-yellow/);

  // Step 7: Switch to different session (simulated by deselecting)
  // In real app, user would click another session
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "other-sess", Title: "Other Session", YoloEnabled: false }),
      makeSession({ ID: "cycle-sess", Title: "Cycle Test", YoloEnabled: true }),
    ],
  });

  // Click on other session
  await page.getByText("Other Session").first().click();
  await page.waitForTimeout(200);

  // YOLO button should be OFF (default for new session)
  await expect(yoloToolbarBtn).not.toHaveClass(/bg-yellow/);

  // Step 8: Switch back to original session
  await page.getByText("Cycle Test").first().click();
  await page.waitForTimeout(200);

  // YOLO should be restored from backend!
  await expect(yoloToolbarBtn).toHaveClass(/bg-yellow/);

  // Step 9: Open modal to verify restored state
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible();
  await expect(knob).toHaveClass(/translate-x-7/);

  // Step 10: Toggle YOLO OFF
  await toggle.click();
  await page.waitForTimeout(200);

  // Verify UI updates to OFF state
  await expect(knob).toHaveClass(/translate-x-1/);
  await expect(toggle).toHaveClass(/bg-surface/);
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible();

  // Step 11: Verify set_yolo was sent to backend
  const setYoloCmd2 = await waitForWSSend(page, "set_yolo");
  expect((setYoloCmd2.payload as { enabled: boolean }).enabled).toBe(false);

  // Step 12: Backend confirms the change
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "cycle-sess", Title: "Cycle Test", YoloEnabled: false }),
  });

  // Step 13: Close modal
  await page.locator("button").filter({ hasText: "Close" }).click();

  // Step 14: Verify toolbar button shows YOLO is OFF
  await expect(yoloToolbarBtn).not.toHaveClass(/bg-yellow/);

  // Step 15: Switch sessions and back to verify persistence
  await page.getByText("Other Session").first().click();
  await page.waitForTimeout(200);
  await page.getByText("Cycle Test").first().click();
  await page.waitForTimeout(200);

  // YOLO should still be OFF (persisted from backend)
  await expect(yoloToolbarBtn).not.toHaveClass(/bg-yellow/);

  // Open modal to verify
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible();
  await expect(knob).toHaveClass(/translate-x-1/);
});
