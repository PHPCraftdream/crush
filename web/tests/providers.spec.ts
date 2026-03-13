import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

/** Config with a single custom provider. */
function makeConfigWithProvider(overrides: Record<string, unknown> = {}) {
  return makeConfig({
    providers: {
      anthropic: {
        name: "Anthropic",
        enabled: true,
        models: [
          { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
        ],
      },
      ollama: {
        name: "Ollama",
        enabled: true,
        isCustom: true,
        baseUrl: "http://localhost:11434/v1/",
        type: "openai-compat",
        models: [{ id: "qwen3:30b", name: "Qwen 3 30B" }],
        ...overrides,
      },
    },
  });
}

async function openProvidersModal(page: Parameters<typeof sendMockWSMessage>[0]) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toBeVisible({ timeout: 3000 });
}

// ── Open / close ─────────────────────────────────────────────────────────────

test("Providers modal closes on Escape", async ({ page }) => {
  await openProvidersModal(page);
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("providers-modal")).not.toBeVisible({ timeout: 3000 });
});

test("Providers modal closes on backdrop click", async ({ page }) => {
  await openProvidersModal(page);
  await page.mouse.click(10, 10);
  await expect(page.getByTestId("providers-modal")).not.toBeVisible({ timeout: 3000 });
});

// ── Provider display ─────────────────────────────────────────────────────────

test("Providers modal shows provider base URL", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfigWithProvider(),
  });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toBeVisible({ timeout: 3000 });
  await expect(
    page.getByText("http://localhost:11434/v1/", { exact: false })
  ).toBeVisible({ timeout: 3000 });
});

test("Providers modal shows model count", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: { name: "Anthropic", enabled: true, models: [] },
        ollama: {
          name: "Ollama",
          enabled: true,
          isCustom: true,
          baseUrl: "http://localhost:11434/v1/",
          type: "openai-compat",
          models: [
            { id: "qwen3:30b", name: "Qwen 3 30B" },
            { id: "llama3:8b", name: "Llama 3 8B" },
          ],
        },
      },
    }),
  });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toBeVisible({ timeout: 3000 });
  // The row subtitle shows "2 models"
  await expect(page.getByText(/2 model/, { exact: false })).toBeVisible({
    timeout: 3000,
  });
});

// ── Edit provider ────────────────────────────────────────────────────────────

test("Edit provider button opens edit form", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfigWithProvider(),
  });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toContainText("Ollama", { timeout: 3000 });
  await page.getByTitle("Edit provider").click();
  await expect(page.getByText("Edit: ollama", { exact: false })).toBeVisible({
    timeout: 3000,
  });
});

test("Edit provider form has ID field disabled", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfigWithProvider(),
  });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toContainText("Ollama", { timeout: 3000 });
  await page.getByTitle("Edit provider").click();
  await expect(page.getByText("Edit: ollama", { exact: false })).toBeVisible({
    timeout: 3000,
  });
  // The Provider ID input is disabled when editing
  const idInput = page.getByPlaceholder("e.g. ollama", { exact: true });
  await expect(idInput).toBeDisabled({ timeout: 3000 });
});

test("Edit provider sends update_custom_provider", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfigWithProvider(),
  });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toContainText("Ollama", { timeout: 3000 });
  await page.getByTitle("Edit provider").click();
  await expect(page.getByText("Edit: ollama", { exact: false })).toBeVisible({
    timeout: 3000,
  });
  // Change the display name
  const nameInput = page.getByPlaceholder("e.g. Ollama", { exact: true });
  await nameInput.clear();
  await nameInput.fill("Ollama Local");
  await page.getByRole("button", { name: "Update Provider" }).click();
  const msg = await waitForWSSend(page, "update_custom_provider");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.oldId).toBe("ollama");
});

// ── Add provider form validation ─────────────────────────────────────────────

test("Add provider form validates empty ID", async ({ page }) => {
  await openProvidersModal(page);
  await page.getByTestId("providers-modal-add").click();
  // Fill base URL but leave ID empty
  await page
    .getByPlaceholder("e.g. http://localhost:11434/v1/")
    .fill("http://localhost:11434/v1/");
  const submitBtn = page.getByRole("button", { name: "Add Provider", exact: true });
  await expect(submitBtn).toBeDisabled({ timeout: 3000 });
});

test("Add provider form validates invalid base URL", async ({ page }) => {
  await openProvidersModal(page);
  await page.getByTestId("providers-modal-add").click();
  // Fill a valid provider ID and name
  await page
    .getByPlaceholder("e.g. ollama", { exact: true })
    .fill("myprovider");
  await page.getByPlaceholder("e.g. Ollama", { exact: true }).fill("My Provider");
  // Fill base URL without http scheme
  await page
    .getByPlaceholder("e.g. http://localhost:11434/v1/")
    .fill("localhost:11434/v1/");
  // Fill model fields so canSubmit would otherwise pass
  await page.getByPlaceholder("ID (e.g. qwen3:30b)").first().fill("model-id");
  await page.getByPlaceholder("Display name").first().fill("Model Name");
  // Attempt to submit
  await page.getByRole("button", { name: "Add Provider", exact: true }).click();
  await expect(
    page.getByText("Base URL must start with http://", { exact: false })
  ).toBeVisible({ timeout: 3000 });
});

test("Add provider form with model saves correctly", async ({ page }) => {
  await openProvidersModal(page);
  await page.getByTestId("providers-modal-add").click();
  await page
    .getByPlaceholder("e.g. ollama", { exact: true })
    .fill("localprovider");
  await page
    .getByPlaceholder("e.g. Ollama", { exact: true })
    .fill("Local Provider");
  await page
    .getByPlaceholder("e.g. http://localhost:11434/v1/")
    .fill("http://localhost:8080/v1/");
  await page.getByPlaceholder("ID (e.g. qwen3:30b)").first().fill("local-model");
  await page.getByPlaceholder("Display name").first().fill("Local Model");
  await page.getByRole("button", { name: "Add Provider", exact: true }).click();
  const msg = await waitForWSSend(page, "add_custom_provider");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.id).toBe("localprovider");
  expect(payload.baseUrl).toBe("http://localhost:8080/v1/");
  const models = payload.models as Array<Record<string, unknown>>;
  expect(Array.isArray(models)).toBe(true);
  expect(models.length).toBeGreaterThan(0);
  expect(models[0].id).toBe("local-model");
  expect(models[0].name).toBe("Local Model");
});

// ── Remove provider ──────────────────────────────────────────────────────────

test("Remove provider - cancel hides confirm", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfigWithProvider(),
  });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toContainText("Ollama", { timeout: 3000 });
  await page.getByTitle("Remove provider").click();
  await expect(page.getByRole("button", { name: "Yes" })).toBeVisible({
    timeout: 3000,
  });
  await page.getByRole("button", { name: "No" }).click();
  await expect(page.getByRole("button", { name: "Yes" })).not.toBeVisible({
    timeout: 3000,
  });
});

// ── API key badge ────────────────────────────────────────────────────────────

test("Provider with API key shows Key set badge", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: { name: "Anthropic", enabled: true, models: [] },
        myapi: {
          name: "My API",
          enabled: true,
          isCustom: true,
          baseUrl: "http://localhost:9000/v1/",
          type: "openai-compat",
          apiKeySet: true,
          models: [{ id: "model-a", name: "Model A" }],
        },
      },
    }),
  });
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toBeVisible({ timeout: 3000 });
  await expect(page.getByText("Key set", { exact: false })).toBeVisible({
    timeout: 3000,
  });
});
