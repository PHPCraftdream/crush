import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Theme toggle visibility ──────────────────────────────────────────────────

test("Theme toggle button visible in header", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "light" }),
  });
  await expect(
    page.getByTitle("Switch to dark theme")
  ).toBeVisible({ timeout: 3000 });
});

// ── Light → dark ─────────────────────────────────────────────────────────────

test("Clicking theme toggle sends set_theme command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "light" }),
  });
  await expect(page.getByTitle("Switch to dark theme")).toBeVisible({
    timeout: 3000,
  });
  await page.getByTitle("Switch to dark theme").click();
  const msg = await waitForWSSend(page, "set_theme");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.theme).toBe("dark");
});

// ── Dark theme config ────────────────────────────────────────────────────────

test("Dark theme config shows light toggle", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "dark" }),
  });
  await expect(
    page.getByTitle("Switch to light theme")
  ).toBeVisible({ timeout: 3000 });
});

test("Clicking theme toggle in dark mode sends light theme", async ({
  page,
}) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "dark" }),
  });
  await expect(page.getByTitle("Switch to light theme")).toBeVisible({
    timeout: 3000,
  });
  await page.getByTitle("Switch to light theme").click();
  const msg = await waitForWSSend(page, "set_theme");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.theme).toBe("light");
});

// ── Dark class on document ───────────────────────────────────────────────────

test("Theme applies dark class to document", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "dark" }),
  });
  // Wait for react to apply the class
  await page.waitForFunction(
    () => document.documentElement.classList.contains("dark"),
    { timeout: 5000 }
  );
  const hasDark = await page.evaluate(() =>
    document.documentElement.classList.contains("dark")
  );
  expect(hasDark).toBe(true);
});
