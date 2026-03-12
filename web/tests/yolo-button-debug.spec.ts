/**
 * Yolo button debugging tests.
 *
 * These tests help identify why the YOLO button is not working.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  await page.addInitScript(() => localStorage.removeItem("crush_yolo"));
});

test("debug: check what elements are visible on initial load", async ({ page }) => {
  await page.goto("/");

  // Wait for page to load
  await page.waitForTimeout(1000);

  // Get all text content
  const bodyText = await page.evaluate(() => document.body.innerText);
  console.log("Page text:", bodyText.substring(0, 500));

  // Check for header
  const headerVisible = await page.locator("header").isVisible();
  console.log("Header visible:", headerVisible);

  // Check for Chat component
  const chatVisible = await page.locator("class*=Chat").isVisible().catch(() => false);
  console.log("Chat visible:", chatVisible);
});

test("debug: check if YOLO button appears after session is selected", async ({ page }) => {
  await page.goto("/");

  // Send sessions
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "test-sess", Title: "Test Session" })],
  });

  // Wait for session to appear
  await expect(page.getByText("Test Session").first()).toBeVisible({ timeout: 3000 });

  // Click on session
  await page.getByText("Test Session").first().click();

  // Wait a bit
  await page.waitForTimeout(500);

  // Check all buttons
  const allButtons = await page.locator("button").all();
  console.log("Total buttons:", allButtons.length);

  for (let i = 0; i < allButtons.length; i++) {
    const text = await allButtons[i].innerText();
    const title = await allButtons[i].getAttribute("title");
    console.log("Button " + i + ": text=" + text + ", title=" + title);
  }

  // Check for YOLO button specifically
  const yoloButton = page.locator("button").filter({ hasText: "Yolo" });
  const yoloCount = await yoloButton.count();
  console.log("YOLO buttons found:", yoloCount);

  if (yoloCount > 0) {
    const yoloText = await yoloButton.first().innerText();
    const yoloTitle = await yoloButton.first().getAttribute("title");
    console.log("YOLO button found: text=" + yoloText + ", title=" + yoloTitle);
  }
});

test("debug: check if ChatToolbar is rendered", async ({ page }) => {
  await page.goto("/");

  // Send sessions
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "test-sess", Title: "Test Session" })],
  });

  await expect(page.getByText("Test Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Test Session").first().click();

  await page.waitForTimeout(500);

  // Check for ChatToolbar elements
  const compactButton = page.locator("button").filter({ hasText: "Compact" });
  const compactCount = await compactButton.count();
  console.log("Compact buttons:", compactCount);

  // Check for toolbar container
  const toolbarDiv = page.locator("div").filter({ hasText: "Compact" });
  const toolbarVisible = await toolbarDiv.isVisible().catch(() => false);
  console.log("Toolbar visible:", toolbarVisible);
});
