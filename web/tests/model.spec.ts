import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Model display ──────────────────────────────────────────────────────────

test("header shows model name from config without session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig(),
  });
  // When no session selected, shows static badge with "large" model
  await expect(page.getByText("claude-opus-4")).toBeVisible({ timeout: 2000 });
});

test("header shows model selector button when session is active", async ({
  page,
}) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "m-sel-1", Title: "Model Session" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig(),
  });
  await page.getByText("Model Session").first().click();
  // The model name appears in a clickable button with dropdown arrow
  await expect(
    page.locator("header button").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });
});

// ── Dropdown open/close ────────────────────────────────────────────────────

test("clicking model button opens dropdown", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "dd-1", Title: "DD Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("DD Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("DD Session").first().click();
  await expect(page.locator("header button").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  await page.locator("header button").filter({ hasText: "claude-opus-4" }).click();
  await expect(page.getByPlaceholder("Search models…")).toBeVisible({ timeout: 2000 });
});

test("model dropdown shows all available models", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "all-1", Title: "All Models" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("All Models").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("All Models").first().click();
  await expect(page.locator("header button").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  await page.locator("header button").filter({ hasText: "claude-opus-4" }).click();
  // Both models should appear in dropdown
  await expect(page.getByText("claude-haiku-4").first()).toBeVisible({ timeout: 2000 });
  // Provider info shown below each model name
  await expect(page.locator("div.absolute").getByText("anthropic").first()).toBeVisible();
});

test("model dropdown closes on outside click", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "cls-1", Title: "Close Test" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Close Test").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Close Test").first().click();
  await expect(page.locator("header button").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  await page.locator("header button").filter({ hasText: "claude-opus-4" }).click();
  await expect(page.getByPlaceholder("Search models…")).toBeVisible({ timeout: 2000 });

  // Click outside
  await page.locator("header h1").click();
  await expect(page.getByPlaceholder("Search models…")).not.toBeVisible({ timeout: 2000 });
});

// ── Search ─────────────────────────────────────────────────────────────────

test("search filters models by name", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "srch-1", Title: "Search Session" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          models: [
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
        openai: { models: [{ id: "gpt-4o", name: "gpt-4o" }] },
      },
    }),
  });
  await expect(page.getByText("Search Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Search Session").first().click();
  await expect(page.locator("header button").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });
  await page.locator("header button").filter({ hasText: "claude-opus-4" }).click();

  await page.getByPlaceholder("Search models…").fill("haiku");
  await expect(page.getByText("claude-haiku-4").first()).toBeVisible({ timeout: 2000 });
  // gpt-4o doesn't match "haiku"
  await expect(page.locator("div.absolute").getByText("gpt-4o")).not.toBeVisible();
});

test("search shows no results message when nothing matches", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "nores-1", Title: "No Results" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("No Results").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("No Results").first().click();
  await expect(page.locator("header button").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });
  await page.locator("header button").filter({ hasText: "claude-opus-4" }).click();

  await page.getByPlaceholder("Search models…").fill("xyznonexistent");
  await expect(page.getByText("No models found")).toBeVisible({ timeout: 2000 });
});

// ── Model selection ────────────────────────────────────────────────────────

test("selecting a model from dropdown updates header display", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "pick-1", Title: "Pick Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Pick Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Pick Session").first().click();
  await expect(page.locator("header button").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  // Open dropdown and select "small" model
  await page.locator("header button").filter({ hasText: "claude-opus-4" }).click();
  // Click the claude-haiku-4 option in the dropdown
  await page.locator("div.absolute").getByText("claude-haiku-4").click();

  // Header large model button now shows the selected model
  await expect(
    page.locator("header button").filter({ hasText: "claude-haiku-4" }).first()
  ).toBeVisible({ timeout: 2000 });
});

// ── Per-session independence ───────────────────────────────────────────────

test("each session has independent model selection", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "ind-1", Title: "Session One" }),
      makeSession({ ID: "ind-2", Title: "Session Two" }),
    ],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });

  // Select Session One and pick "small" model
  await expect(page.getByText("Session One")).toBeVisible({ timeout: 3000 });
  await page.getByText("Session One").click();
  await expect(page.locator("header button").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });
  await page.locator("header button").filter({ hasText: "claude-opus-4" }).click();
  await page.locator("div.absolute").getByText("claude-haiku-4").click();
  await expect(
    page.locator("header button").filter({ hasText: "claude-haiku-4" }).first()
  ).toBeVisible({ timeout: 2000 });

  // Switch to Session Two — should show default "large" model
  await page.getByText("Session Two").click();
  await expect(
    page.locator("header button").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });

  // Switch back to Session One — should still show "small"
  await page.getByText("Session One").click();
  await expect(
    page.locator("header button").filter({ hasText: "claude-haiku-4" }).first()
  ).toBeVisible({ timeout: 2000 });
});

// ── Settings model panel ───────────────────────────────────────────────────

test("settings shows session model dropdowns when session is active", async ({
  page,
}) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "st-1", Title: "Settings Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Settings Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Settings Session").first().click();
  await page.getByTitle("Settings").click();

  await expect(page.getByText("Session Models")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Large model")).toBeVisible();
  await expect(page.getByText("Small model")).toBeVisible();
});

test("settings model select changes displayed model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "st-2", Title: "Model Change" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Model Change").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Model Change").first().click();
  await page.getByTitle("Settings").click();

  // Change large model to "small" key
  const largeSelect = page.locator("select").first();
  await largeSelect.selectOption("small");

  // Close settings and verify header large model button shows new model
  await page.locator(".fixed.inset-0.z-40").click();
  await expect(
    page.locator("header button").filter({ hasText: "claude-haiku-4" }).first()
  ).toBeVisible({ timeout: 2000 });
});

// ── Small model selector ────────────────────────────────────────────────────

test("header shows small model selector button when session is active", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sm-1", Title: "Small Model Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Small Model Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Small Model Session").first().click();
  // Small model button with ⚡ icon
  await expect(
    page.locator("header button[title='Small (fast) model']")
  ).toBeVisible({ timeout: 2000 });
});

test("clicking small model button opens its own dropdown", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sm-2", Title: "Small DD" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Small DD").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Small DD").first().click();
  await page.locator("header button[title='Small (fast) model']").click();
  await expect(page.getByPlaceholder("Search models…")).toBeVisible({ timeout: 2000 });
});

test("selecting from small model dropdown updates small selector", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sm-3", Title: "Small Pick" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          models: [
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
        openai: { models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini" }] },
      },
    }),
  });
  await expect(page.getByText("Small Pick").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Small Pick").first().click();
  // Open small model dropdown
  await page.locator("header button[title='Small (fast) model']").click();
  // Pick gpt-4o-mini
  await page.locator("div.absolute").getByText("gpt-4o-mini").click();
  // Small model button now shows gpt-4o-mini
  await expect(
    page.locator("header button[title='Small (fast) model']").filter({ hasText: "gpt-4o-mini" })
  ).toBeVisible({ timeout: 2000 });
  // Large model button unchanged
  await expect(
    page.locator("header button[title='Large (strong) model']").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });
});
