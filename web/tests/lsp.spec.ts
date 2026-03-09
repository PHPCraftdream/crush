import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig } from "./helpers/fixtures";

const goplsServer = {
  name: "gopls",
  state: "ready",
  disabled: false,
  command: "gopls",
  args: [] as string[],
  fileTypes: ["go"],
  diagnosticCount: 0,
};

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function openLSPModal(page: Parameters<typeof sendMockWSMessage>[0]) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(
    page.getByRole("heading", { name: "LSP Servers" })
  ).toBeVisible({ timeout: 3000 });
}

// ── Open / close ─────────────────────────────────────────────────────────────

test("LSP button opens LSP settings modal", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(
    page.getByRole("heading", { name: "LSP Servers" })
  ).toBeVisible({ timeout: 3000 });
});

test("LSP modal closes on Escape", async ({ page }) => {
  await openLSPModal(page);
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("heading", { name: "LSP Servers" })
  ).not.toBeVisible({ timeout: 3000 });
});

test("LSP modal closes on backdrop click", async ({ page }) => {
  await openLSPModal(page);
  await page.mouse.click(10, 10);
  await expect(
    page.getByRole("heading", { name: "LSP Servers" })
  ).not.toBeVisible({ timeout: 3000 });
});

// ── Empty / populated state ──────────────────────────────────────────────────

test("LSP modal shows empty state when no servers", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { servers: [] },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(
    page.getByRole("heading", { name: "LSP Servers" })
  ).toBeVisible({ timeout: 3000 });
  await expect(page.getByText("No LSP servers")).toBeVisible({ timeout: 3000 });
});

test("LSP modal shows server list", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { servers: [goplsServer] },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(
    page.getByRole("heading", { name: "LSP Servers" })
  ).toBeVisible({ timeout: 3000 });
  await expect(page.getByText("gopls", { exact: true }).first()).toBeVisible({
    timeout: 3000,
  });
});

// ── Status badges ────────────────────────────────────────────────────────────

test("LSP server status shows Ready badge", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { servers: [goplsServer] },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(page.getByText("Ready")).toBeVisible({ timeout: 3000 });
});

test("LSP server status shows Off badge for disabled server", async ({
  page,
}) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: {
      servers: [{ ...goplsServer, disabled: true }],
    },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(page.getByText("Off")).toBeVisible({ timeout: 3000 });
});

// ── Toggle enable/disable ────────────────────────────────────────────────────

test("LSP disable toggle sends set_lsp_disabled command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { servers: [goplsServer] },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(page.getByTitle("Disable")).toBeVisible({ timeout: 3000 });
  await page.getByTitle("Disable").click();
  const msg = await waitForWSSend(page, "set_lsp_disabled");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.name).toBe("gopls");
  expect(payload.disabled).toBe(true);
});

test("LSP enable toggle sends set_lsp_disabled command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: {
      servers: [{ ...goplsServer, disabled: true }],
    },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(page.getByTitle("Enable")).toBeVisible({ timeout: 3000 });
  await page.getByTitle("Enable").click();
  const msg = await waitForWSSend(page, "set_lsp_disabled");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.name).toBe("gopls");
  expect(payload.disabled).toBe(false);
});

// ── Remove server ────────────────────────────────────────────────────────────

test("LSP remove server shows confirm then sends remove_lsp_server", async ({
  page,
}) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { servers: [goplsServer] },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(page.getByTitle("Remove server")).toBeVisible({ timeout: 3000 });
  await page.getByTitle("Remove server").click();
  await expect(page.getByRole("button", { name: "Yes" })).toBeVisible({
    timeout: 3000,
  });
  await page.getByRole("button", { name: "Yes" }).click();
  const msg = await waitForWSSend(page, "remove_lsp_server");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.name).toBe("gopls");
});

test("LSP remove server cancel hides confirm", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { servers: [goplsServer] },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(page.getByTitle("Remove server")).toBeVisible({ timeout: 3000 });
  await page.getByTitle("Remove server").click();
  await expect(page.getByRole("button", { name: "Yes" })).toBeVisible({
    timeout: 3000,
  });
  await page.getByRole("button", { name: "No" }).click();
  await expect(page.getByRole("button", { name: "Yes" })).not.toBeVisible({
    timeout: 3000,
  });
});

// ── Add LSP server form ──────────────────────────────────────────────────────

test("Add LSP server form shows on button click", async ({ page }) => {
  await openLSPModal(page);
  await page.getByRole("button", { name: /Add LSP server/i }).click();
  await expect(page.locator('textarea[spellcheck="false"]')).toBeVisible({ timeout: 3000 });
});

test("Add LSP server sends add_lsp_server command", async ({ page }) => {
  await openLSPModal(page);
  await page.getByRole("button", { name: /Add LSP server/i }).click();
  await expect(page.locator('textarea[spellcheck="false"]')).toBeVisible({ timeout: 3000 });
  await page.locator('textarea[spellcheck="false"]').fill(
    JSON.stringify({
      name: "pyright",
      command: "pyright-langserver",
      args: ["--stdio"],
    })
  );
  await page.getByRole("button", { name: "Add Server" }).click();
  const msg = await waitForWSSend(page, "add_lsp_server");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.name).toBe("pyright");
  expect(payload.command).toBe("pyright-langserver");
});

test("Add LSP server shows error for invalid JSON", async ({ page }) => {
  await openLSPModal(page);
  await page.getByRole("button", { name: /Add LSP server/i }).click();
  await expect(page.locator('textarea[spellcheck="false"]')).toBeVisible({ timeout: 3000 });
  await page.locator('textarea[spellcheck="false"]').fill("not json");
  await page.getByRole("button", { name: "Add Server" }).click();
  await expect(page.getByText("Invalid JSON")).toBeVisible({ timeout: 3000 });
});

test("Add LSP server shows error when name missing", async ({ page }) => {
  await openLSPModal(page);
  await page.getByRole("button", { name: /Add LSP server/i }).click();
  await expect(page.locator('textarea[spellcheck="false"]')).toBeVisible({ timeout: 3000 });
  await page.locator('textarea[spellcheck="false"]').fill(
    JSON.stringify({ command: "pyright-langserver" })
  );
  await page.getByRole("button", { name: "Add Server" }).click();
  await expect(page.getByText('"name" field is required')).toBeVisible({
    timeout: 3000,
  });
});

// ── File types expansion ─────────────────────────────────────────────────────

test("File types shown when expanded", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: {
      servers: [{ ...goplsServer, fileTypes: ["go", "mod"] }],
    },
  });
  await page.getByRole("button", { name: "LSP" }).click();
  await expect(page.getByText("gopls", { exact: true }).first()).toBeVisible({
    timeout: 3000,
  });
  // Click the chevron button to expand file types (title "Show file types")
  await page.getByTitle("Show file types").click();
  await expect(page.getByText("go", { exact: true })).toBeVisible({
    timeout: 3000,
  });
});
