import { test, expect } from "@playwright/test";

test("shows login page initially", async ({ page }) => {
  // Intercept auth check to return not authed
  await page.route("/auth/check", (route) => route.fulfill({ status: 401 }));
  await page.goto("/");
  await expect(page.locator('input[type="password"]')).toBeVisible();
});

test("shows Connect button on login page", async ({ page }) => {
  await page.route("/auth/check", (route) => route.fulfill({ status: 401 }));
  await page.goto("/");
  await expect(page.getByText("Connect")).toBeVisible();
});

test("shows error for invalid token", async ({ page }) => {
  await page.route("/auth/check", (route) => route.fulfill({ status: 401 }));
  await page.route("/auth", (route) =>
    route.fulfill({ status: 401, body: "Unauthorized" })
  );
  await page.goto("/");
  await page.locator('input[type="password"]').fill("wrong-token");
  await page.getByText("Connect").click();
  await expect(page.getByText("Invalid token")).toBeVisible();
});

test("Connect button is disabled when token is empty", async ({ page }) => {
  await page.route("/auth/check", (route) => route.fulfill({ status: 401 }));
  await page.goto("/");
  const btn = page.getByText("Connect");
  await expect(btn).toBeDisabled();
});

test("navigates to app on valid token", async ({ page }) => {
  await page.route("/auth/check", (route) => route.fulfill({ status: 401 }));
  await page.route("/auth", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
  // Block WS connection from erroring out visibly
  await page.goto("/");
  await page.locator('input[type="password"]').fill("valid-token");
  await page.getByText("Connect").click();
  // After successful auth, login page should disappear
  await expect(page.locator('input[type="password"]')).not.toBeVisible({ timeout: 3000 });
});
