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
});

// ── Visibility ────────────────────────────────────────────────────────────────

test("Yolo button is always visible in the header", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator("header").getByText("Yolo")).toBeVisible({ timeout: 3000 });
});

test("Yolo button shows lock icon by default (inactive)", async ({ page }) => {
  await page.goto("/");
  // 🔒 is the inactive icon
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

// ── Enabling yolo ──────────────────────────────────────────────────────────

test("clicking Yolo button sends set_yolo with enabled: true", async ({ page }) => {
  await page.goto("/");
  await page.locator("header").getByText("Yolo").click();
  const cmd = await waitForWSSend(page, "set_yolo");
  expect((cmd.payload as { enabled: boolean }).enabled).toBe(true);
});

test("Yolo button shows lightning icon after enabling", async ({ page }) => {
  await page.goto("/");
  await page.locator("header").getByText("Yolo").click();
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });
});

// ── Disabling yolo ─────────────────────────────────────────────────────────

test("clicking Yolo button again sends set_yolo with enabled: false", async ({ page }) => {
  await page.goto("/");
  // Enable first
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");

  // Now disable — need to wait for the second set_yolo command
  await page.locator("header").getByText("Yolo").click();

  const secondCmd = await page.waitForFunction(
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
  const cmd = await secondCmd.jsonValue() as { type: string; payload: { enabled: boolean } };
  expect(cmd.payload.enabled).toBe(false);
});

test("Yolo button returns to lock icon after disabling", async ({ page }) => {
  await page.goto("/");
  await page.locator("header").getByText("Yolo").click();
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  await page.locator("header").getByText("Yolo").click();
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

// ── Config initialisation ──────────────────────────────────────────────────

test("config event with yolo: true shows lightning icon", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ yolo: true }),
  });
  // yolo: true in server config with no localStorage override → active
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });
});

test("config event with yolo: false keeps lock icon", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ yolo: false }),
  });
  await expect(page.locator("header").getByText("🔒")).toBeVisible({ timeout: 2000 });
});

// ── LocalStorage persistence ───────────────────────────────────────────────

test("yolo state persists across page reload via localStorage", async ({ page }) => {
  await page.goto("/");
  // Enable yolo
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });

  // Verify localStorage was written
  const stored = await page.evaluate(() => localStorage.getItem("crush_yolo"));
  expect(stored).toBe("true");

  // Reload with mock WS (addInitScript runs again before page load)
  await setupMockWS(page);
  await page.reload();
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );

  // Lightning icon should still be there — loaded from localStorage
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 3000 });
});

test("localStorage stores false when yolo is disabled", async ({ page }) => {
  await page.goto("/");
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");
  // Disable
  await page.locator("header").getByText("Yolo").click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return sent.filter((m) => m.type === "set_yolo").length >= 2;
  }, { timeout: 5000 });

  const stored = await page.evaluate(() => localStorage.getItem("crush_yolo"));
  expect(stored).toBe("false");
});

// ── Tooltip ─────────────────────────────────────────────────────────────────

test("inactive Yolo button has tooltip explaining it requires approval", async ({ page }) => {
  await page.goto("/");
  const btn = page.locator("header button", { hasText: "Yolo" });
  const title = await btn.getAttribute("title");
  expect(title).toContain("OFF");
});

test("active Yolo button has tooltip explaining auto-approval", async ({ page }) => {
  await page.goto("/");
  await page.locator("header").getByText("Yolo").click();
  await waitForWSSend(page, "set_yolo");
  const btn = page.locator("header button", { hasText: "Yolo" });
  const title = await btn.getAttribute("title");
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
  await expect(page.getByPlaceholder("Message… (Enter to send)")).toBeEnabled({ timeout: 2000 });

  // Toggle yolo while session is active
  await page.locator("header").getByText("Yolo").click();
  const cmd = await waitForWSSend(page, "set_yolo");
  expect((cmd.payload as { enabled: boolean }).enabled).toBe(true);
  await expect(page.locator("header").getByText("⚡")).toBeVisible({ timeout: 2000 });
});
