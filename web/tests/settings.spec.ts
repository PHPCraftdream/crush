import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Settings modal ─────────────────────────────────────────────────────────

test("Settings button opens settings modal", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { name: "Settings", exact: true })).toBeVisible({ timeout: 3000 });
  await expect(page.getByText("Enable debug logging", { exact: true })).toBeVisible({ timeout: 2000 });
});

test("Settings modal closes on Escape", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { name: "Settings", exact: true })).toBeVisible({ timeout: 2000 });
  await page.keyboard.press("Escape");
  await expect(page.getByRole("heading", { name: "Settings", exact: true })).not.toBeVisible({ timeout: 2000 });
});

test("Settings modal closes on backdrop click", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { name: "Settings", exact: true })).toBeVisible({ timeout: 2000 });
  await page.mouse.click(10, 10);
  await expect(page.getByRole("heading", { name: "Settings", exact: true })).not.toBeVisible({ timeout: 2000 });
});

// ── Debug toggle ──────────────────────────────────────────────────────────

test("Debug toggle sends set_debug command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ debug: false }),
  });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByText("Enable debug logging", { exact: true })).toBeVisible({ timeout: 2000 });
  await page.getByRole("switch", { name: "Enable debug logging" }).click();
  const msg = await waitForWSSend(page, "set_debug");
  expect((msg.payload as Record<string, unknown>).debug).toBe(true);
});

test("LSP debug toggle sends set_debug command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ debugLsp: false }),
  });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByText("Enable LSP debug logging", { exact: true })).toBeVisible({ timeout: 2000 });
  await page.getByRole("switch", { name: "Enable LSP debug logging" }).click();
  const msg = await waitForWSSend(page, "set_debug");
  expect((msg.payload as Record<string, unknown>).debugLsp).toBe(true);
});

// ── Context paths ─────────────────────────────────────────────────────────

test("Context Paths section shows existing paths", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ contextPaths: ["docs/arch.md", ".cursorrules"] }),
  });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByText("docs/arch.md", { exact: true })).toBeVisible({ timeout: 2000 });
  await expect(page.getByText(".cursorrules", { exact: true })).toBeVisible({ timeout: 2000 });
});

test("Adding a context path sends add_context_path command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Settings" }).click();
  await page.getByPlaceholder("e.g. docs/architecture.md").fill("myfile.md");
  await page.getByPlaceholder("e.g. docs/architecture.md").press("Enter");
  const msg = await waitForWSSend(page, "add_context_path");
  expect((msg.payload as Record<string, unknown>).path).toBe("myfile.md");
});

test("Removing a context path sends remove_context_path command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ contextPaths: ["docs/arch.md"] }),
  });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByText("docs/arch.md", { exact: true })).toBeVisible({ timeout: 2000 });
  await page.getByTitle("Remove").first().click();
  const msg = await waitForWSSend(page, "remove_context_path");
  expect((msg.payload as Record<string, unknown>).path).toBe("docs/arch.md");
});

// ── Skills paths ──────────────────────────────────────────────────────────

test("Skills Paths section shows existing paths", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ skillsPaths: ["./my-skills"] }),
  });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByText("./my-skills", { exact: true })).toBeVisible({ timeout: 2000 });
});

test("Adding a skills path sends add_skills_path command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Settings" }).click();
  await page.getByPlaceholder("e.g. ./project-skills").fill("./new-skills");
  await page.getByPlaceholder("e.g. ./project-skills").press("Enter");
  const msg = await waitForWSSend(page, "add_skills_path");
  expect((msg.payload as Record<string, unknown>).path).toBe("./new-skills");
});

// ── Project initialization ────────────────────────────────────────────────

test("Initialize Project button sends initialize_project command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByRole("button", { name: /Initialize Project/i })).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: /Initialize Project/i }).click();
  await waitForWSSend(page, "initialize_project");
});

// ── Providers modal ───────────────────────────────────────────────────────

test("Providers button opens providers modal", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Providers" }).click();
  await expect(page.getByRole("heading", { name: "Custom Providers" })).toBeVisible({ timeout: 3000 });
});

test("Providers modal shows custom providers", async ({ page }) => {
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
          models: [{ id: "qwen3:30b", name: "Qwen 3 30B" }],
        },
      },
    }),
  });
  await page.getByRole("button", { name: "Providers" }).click();
  await expect(page.getByText("Ollama", { exact: true })).toBeVisible({ timeout: 3000 });
});

test("Providers modal - add custom provider sends command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "Providers" }).click();
  await page.getByRole("button", { name: /Add custom provider/i }).click();
  // Use nth(0) for Provider ID field since placeholders differ by case
  await page.getByPlaceholder("e.g. ollama", { exact: true }).fill("myollama");
  await page.getByPlaceholder("e.g. http://localhost:11434/v1/").fill("http://localhost:11434/v1/");
  await page.getByRole("button", { name: "Add Provider", exact: true }).click();
  const msg = await waitForWSSend(page, "add_custom_provider");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.id).toBe("myollama");
  expect(payload.baseUrl).toBe("http://localhost:11434/v1/");
});

test("Providers modal - remove custom provider sends command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        ollama: {
          name: "Ollama Local",
          enabled: true,
          isCustom: true,
          baseUrl: "http://localhost:11434/v1/",
          type: "openai-compat",
          models: [],
        },
      },
    }),
  });
  await page.getByRole("button", { name: "Providers" }).click();
  await expect(page.getByText("Ollama Local", { exact: true })).toBeVisible({ timeout: 2000 });
  await page.getByTitle("Remove provider").click();
  await page.getByRole("button", { name: "Yes" }).click();
  const msg = await waitForWSSend(page, "remove_custom_provider");
  expect((msg.payload as Record<string, unknown>).id).toBe("ollama");
});

// ── File attachments ──────────────────────────────────────────────────────

test("Attach files button is visible when session active", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [{ ID: "s1", Title: "Test", MessageCount: 0, PromptTokens: 0, CompletionTokens: 0, Cost: 0, Todos: [], CreatedAt: 1700000000000, UpdatedAt: 1700000000000, ParentSessionID: "" }],
  });
  await page.getByText("Test").first().click();
  await expect(page.getByTitle("Attach files")).toBeVisible({ timeout: 2000 });
});

test("File drop shows attachment badge", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [{ ID: "s1", Title: "Attach Session", MessageCount: 0, PromptTokens: 0, CompletionTokens: 0, Cost: 0, Todos: [], CreatedAt: 1700000000000, UpdatedAt: 1700000000000, ParentSessionID: "" }],
  });
  await page.getByText("Attach Session").first().click();
  // Simulate a file drop on the input area
  await page.evaluate(() => {
    const dropArea = document.querySelector("div[class*='rounded-2xl'][class*='bg-base-overlay']");
    if (!dropArea) return;
    const file = new File(["hello"], "test.txt", { type: "text/plain" });
    const dt = new DataTransfer();
    dt.items.add(file);
    const ev = new DragEvent("drop", { dataTransfer: dt, bubbles: true });
    dropArea.dispatchEvent(ev);
  });
  await expect(page.getByText("test.txt")).toBeVisible({ timeout: 3000 });
});
