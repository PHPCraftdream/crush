/**
 * Detailed status bar tests.
 *
 * Covers:
 *  - Connection status dot color (green/red)
 *  - LSP state update replaces existing entry
 *  - Multiple LSP servers displayed
 *  - LSP dot colors for different states
 *  - MCP server statuses and dot colors
 *  - MCP not shown when no servers
 *  - LSP not shown when no servers
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Connection status ──────────────────────────────────────────────────

test("shows Connected with green dot after WS connects", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByText("Connected")).toBeVisible({ timeout: 3000 });
  const dot = page.locator("text=Connected").locator("..").locator("span.rounded-full");
  await expect(dot).toHaveClass(/bg-green/);
});

// ── LSP states ─────────────────────────────────────────────────────────

test("multiple LSP servers displayed", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "gopls", state: "running", diagnosticCount: 0 },
  });
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "tsserver", state: "starting", diagnosticCount: 2 },
  });

  await expect(page.getByText("gopls")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("tsserver")).toBeVisible();
  await expect(page.getByText("(2)")).toBeVisible();
});

test("LSP state update replaces existing entry", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "gopls", state: "starting", diagnosticCount: 0 },
  });
  await expect(page.getByText("gopls")).toBeVisible({ timeout: 2000 });

  // Update same server with new state
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "gopls", state: "running", diagnosticCount: 5 },
  });

  // Still only one "gopls" entry, now with diagnostics
  await expect(page.getByText("gopls")).toBeVisible();
  await expect(page.getByText("(5)")).toBeVisible({ timeout: 2000 });
});

test("LSP running state shows green dot", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "gopls", state: "running", diagnosticCount: 0 },
  });

  const lspItem = page.getByTitle("gopls: running");
  await expect(lspItem).toBeVisible({ timeout: 2000 });
  await expect(lspItem.locator("span.rounded-full")).toHaveClass(/bg-green/);
});

test("LSP starting state shows yellow dot", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "tsserver", state: "starting", diagnosticCount: 0 },
  });

  const lspItem = page.getByTitle("tsserver: starting");
  await expect(lspItem).toBeVisible({ timeout: 2000 });
  await expect(lspItem.locator("span.rounded-full")).toHaveClass(/bg-yellow/);
});

test("LSP error state shows red dot", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "lsp_state",
    payload: { name: "pyright", state: "error", diagnosticCount: 0 },
  });

  const lspItem = page.getByTitle("pyright: error");
  await expect(lspItem).toBeVisible({ timeout: 2000 });
  await expect(lspItem.locator("span.rounded-full")).toHaveClass(/bg-red/);
});

test("LSP section not shown when no servers", async ({ page }) => {
  await page.goto("/");
  // Wait for connection
  await expect(page.getByText("Connected")).toBeVisible({ timeout: 3000 });
  // LSP label should not be present
  await expect(page.getByText("LSP")).not.toBeVisible({ timeout: 1000 });
});

// ── MCP states ─────────────────────────────────────────────────────────

test("MCP connected server shows green dot", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "filesystem", status: "connected" }] },
  });

  const mcpItem = page.getByTitle("filesystem: connected");
  await expect(mcpItem).toBeVisible({ timeout: 2000 });
  await expect(mcpItem.locator("span.rounded-full")).toHaveClass(/bg-green/);
});

test("MCP connecting server shows yellow dot", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "db-server", status: "connecting" }] },
  });

  const mcpItem = page.getByTitle("db-server: connecting");
  await expect(mcpItem).toBeVisible({ timeout: 2000 });
  await expect(mcpItem.locator("span.rounded-full")).toHaveClass(/bg-yellow/);
});

test("MCP error server shows red dot", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "broken", status: "error" }] },
  });

  const mcpItem = page.getByTitle("broken: error");
  await expect(mcpItem).toBeVisible({ timeout: 2000 });
  await expect(mcpItem.locator("span.rounded-full")).toHaveClass(/bg-red/);
});

test("MCP section not shown when no servers", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByText("Connected")).toBeVisible({ timeout: 3000 });
  await expect(page.getByText("MCP")).not.toBeVisible({ timeout: 1000 });
});

test("MCP section not shown when servers array is empty", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [] },
  });
  await expect(page.getByText("MCP")).not.toBeVisible({ timeout: 1000 });
});

test("multiple MCP servers displayed", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: {
      servers: [
        { name: "filesystem", status: "connected" },
        { name: "database", status: "connecting" },
      ],
    },
  });

  await expect(page.getByText("filesystem")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("database")).toBeVisible();
});
