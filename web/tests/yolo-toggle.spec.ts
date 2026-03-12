/**
 * YOLO Toggle TDD Test
 *
 * Reproduces the issue where toggle doesn't update UI
 * because of closure capturing stale yolo value
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

test("YOLO toggle updates UI when clicked", async ({ page }) => {
  await page.goto("/");

  // Create a session
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "toggle-sess", Title: "Toggle Test", YoloEnabled: false })],
  });

  // Click on session to activate it
  await page.getByText("Toggle Test").first().click();
  await page.waitForTimeout(200);

  // Open permissions modal
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();
  await expect(page.getByText("YOLO Mode")).toBeVisible({ timeout: 2000 });

  // Verify initial state - OFF
  const toggle = page.locator(".relative.w-14.h-7").first();
  await expect(toggle).toHaveClass(/bg-surface/);
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible();

  // Click toggle to turn ON
  await toggle.click();
  await page.waitForTimeout(100);

  // CRITICAL: Toggle should move to the right (translate-x-7)
  const knob = page.locator(".absolute.top-1");
  await expect(knob).toHaveClass(/translate-x-7/);

  // Background should be yellow
  await expect(toggle).toHaveClass(/bg-yellow/);

  // Text should change
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible();

  // Send session_updated from backend to confirm the change
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "toggle-sess", Title: "Toggle Test", YoloEnabled: true }),
  });

  // Close and reopen modal to verify persistence
  await page.locator("button").filter({ hasText: "Close" }).click();
  await page.locator(".btn-toolbar").filter({ hasText: "Yolo" }).click();

  // Should still be ON
  await expect(page.getByText("All permissions are automatically approved")).toBeVisible();
  await expect(toggle).toHaveClass(/bg-yellow/);
  await expect(knob).toHaveClass(/translate-x-7/);

  // Toggle back to OFF
  await toggle.click();
  await page.waitForTimeout(100);

  // Should move back to left
  await expect(knob).toHaveClass(/translate-x-1/);
  await expect(toggle).toHaveClass(/bg-surface/);
  await expect(page.getByText("Individual permission rules are applied")).toBeVisible();
});
