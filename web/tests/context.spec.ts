/**
 * Context window fill percentage and token usage tests.
 *
 * Covers:
 *  - Header shows formatted token count for active session
 *  - Context % displayed when model has contextWindow
 *  - No context % shown when model has no contextWindow
 *  - Color coding: green (<60%), yellow (60-84%), red (≥85%)
 *  - Tooltip shows exact token / context counts
 *  - Token display updates when session receives session_updated
 *  - Zero tokens: badge not shown
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Token count display ───────────────────────────────────────────────────────

test("token count appears in header when session has tokens", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ctx-1", Title: "Token Session", PromptTokens: 1200, CompletionTokens: 800 })],
  });
  await expect(page.getByText("Token Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Token Session").first().click();
  // 1200 + 800 = 2000 → formatTokens → "2.0k"
  await expect(page.locator("header").getByText(/2\.0k/)).toBeVisible({ timeout: 2000 });
});

test("token count not shown when session has zero tokens", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ctx-zero", Title: "Empty Tokens", PromptTokens: 0, CompletionTokens: 0 })],
  });
  await expect(page.getByText("Empty Tokens").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Empty Tokens").first().click();
  // No token badge shown for zero tokens
  await expect(page.locator("header span[title*='token']")).not.toBeVisible({ timeout: 2000 });
});

test("token count shows millions for large usage", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ctx-m", Title: "Mega Tokens", PromptTokens: 1_500_000, CompletionTokens: 600_000 })],
  });
  await expect(page.getByText("Mega Tokens").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Mega Tokens").first().click();
  // 2.1M tokens
  await expect(page.locator("header").getByText(/2\.1M/)).toBeVisible({ timeout: 2000 });
});

// ── Context percentage ────────────────────────────────────────────────────────

test("context % shown when model has contextWindow", async ({ page }) => {
  await page.goto("/");
  // Session uses 100k tokens; model context = 200k → 50%
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-pct",
      Title: "Pct Session",
      PromptTokens: 80_000,
      CompletionTokens: 20_000,
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-opus-4",
    })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig(), // anthropic/claude-opus-4 has contextWindow: 200000
  });
  await expect(page.getByText("Pct Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Pct Session").first().click();
  // 100k / 200k = 50%
  await expect(page.locator("header").getByText("50%")).toBeVisible({ timeout: 2000 });
});

test("context % not shown when model has no contextWindow", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-no-win",
      Title: "No Window",
      PromptTokens: 10_000,
      CompletionTokens: 5_000,
      LargeModelProvider: "unknown",
      LargeModelID: "mystery-model",
    })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        unknown: { models: [{ id: "mystery-model", name: "mystery-model" }] },
      },
    }),
  });
  await expect(page.getByText("No Window").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("No Window").first().click();
  // Tokens shown but no %
  await expect(page.locator("header").getByText(/15\.0k/)).toBeVisible({ timeout: 2000 });
  await expect(page.locator("header").getByText(/%/)).not.toBeVisible({ timeout: 1000 });
});

// ── Color coding ──────────────────────────────────────────────────────────────

test("context % has green color class when below 60%", async ({ page }) => {
  await page.goto("/");
  // 50k / 200k = 25% → green
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-green",
      Title: "Green Ctx",
      PromptTokens: 40_000,
      CompletionTokens: 10_000,
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-opus-4",
    })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Green Ctx").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Green Ctx").first().click();
  const pctEl = page.locator("header").getByText("25%");
  await expect(pctEl).toBeVisible({ timeout: 2000 });
  await expect(pctEl).toHaveClass(/text-green/);
});

test("context % has yellow color class between 60% and 85%", async ({ page }) => {
  await page.goto("/");
  // 140k / 200k = 70% → yellow
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-yellow",
      Title: "Yellow Ctx",
      PromptTokens: 100_000,
      CompletionTokens: 40_000,
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-opus-4",
    })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Yellow Ctx").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Yellow Ctx").first().click();
  const pctEl = page.locator("header").getByText("70%");
  await expect(pctEl).toBeVisible({ timeout: 2000 });
  await expect(pctEl).toHaveClass(/text-yellow/);
});

test("context % has red color class at 85% or above", async ({ page }) => {
  await page.goto("/");
  // 180k / 200k = 90% → red
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-red",
      Title: "Red Ctx",
      PromptTokens: 150_000,
      CompletionTokens: 30_000,
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-opus-4",
    })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Red Ctx").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Red Ctx").first().click();
  const pctEl = page.locator("header").getByText("90%");
  await expect(pctEl).toBeVisible({ timeout: 2000 });
  await expect(pctEl).toHaveClass(/text-red/);
});

test("context % capped at 100% even if tokens exceed context window", async ({ page }) => {
  await page.goto("/");
  // 220k / 200k = 110% → capped to 100%
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-cap",
      Title: "Capped Ctx",
      PromptTokens: 200_000,
      CompletionTokens: 20_000,
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-opus-4",
    })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Capped Ctx").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Capped Ctx").first().click();
  await expect(page.locator("header").getByText("100%")).toBeVisible({ timeout: 2000 });
});

// ── Tooltip ───────────────────────────────────────────────────────────────────

test("token badge tooltip shows exact token count when no context window", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-tip1",
      Title: "Tip Session",
      PromptTokens: 1_200,
      CompletionTokens: 800,
    })],
  });
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-tip1",
      Title: "Tip Session",
      PromptTokens: 1_200,
      CompletionTokens: 800,
    })],
  });
  await expect(page.getByText("Tip Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Tip Session").first().click();
  // No model with contextWindow in session → title ends with "tokens used"
  const badge = page.locator("header span[title*='tokens']");
  await expect(badge).toBeVisible({ timeout: 2000 });
  const title = await badge.getAttribute("title");
  expect(title).toContain("tokens");
});

test("token badge tooltip shows exact/context ratio when context window available", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-tip2",
      Title: "Ratio Session",
      PromptTokens: 80_000,
      CompletionTokens: 20_000,
      LargeModelProvider: "anthropic",
      LargeModelID: "claude-opus-4",
    })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Ratio Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Ratio Session").first().click();
  const badge = page.locator("header span[title*='tokens']");
  await expect(badge).toBeVisible({ timeout: 2000 });
  const title = await badge.getAttribute("title");
  // Should contain both the used tokens and context window
  expect(title).toContain("100,000");
  expect(title).toContain("200,000");
});

// ── Updates on session_updated ────────────────────────────────────────────────

test("token count updates when session_updated arrives with more tokens", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "ctx-upd",
      Title: "Updating Session",
      PromptTokens: 1_000,
      CompletionTokens: 0,
    })],
  });
  await expect(page.getByText("Updating Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Updating Session").first().click();
  await expect(page.locator("header").getByText("1.0k")).toBeVisible({ timeout: 2000 });

  // Server sends updated session with more tokens
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({
      ID: "ctx-upd",
      Title: "Updating Session",
      PromptTokens: 50_000,
      CompletionTokens: 5_000,
    }),
  });
  await expect(page.locator("header").getByText("55.0k")).toBeVisible({ timeout: 2000 });
});
