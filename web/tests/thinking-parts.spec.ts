/**
 * Thinking/reasoning and tool part rendering tests.
 *
 * Covers:
 *  - Thinking part renders as collapsible details/summary
 *  - Thinking content hidden by default (inside <details>)
 *  - Tool call shows "running…" when not finished
 *  - Tool call hides "running…" when finished
 *  - Tool result with IsError shows error badge
 *  - Tool result without error has no error badge
 *  - Finish part renders nothing (null)
 *  - Tool call input is formatted as JSON
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithMessage(
  page: import("@playwright/test").Page,
  msg: ReturnType<typeof makeMessage>
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "tp-sess", Title: "Parts Session" })],
  });
  await expect(page.getByText("Parts Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Parts Session").first().click();
  await sendMockWSMessage(page, { type: "messages_list", payload: [msg] });
}

// ── Thinking part ───────────────────────────────────────────────────────

test("thinking part renders as collapsible with 'Thoughts' label when done", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-1",
    Role: "assistant",
    Parts: [
      { type: "thinking", Thinking: "Let me think about this carefully..." },
      { type: "text", Text: "Here is my answer." },
    ],
  }));

  // When thinking is done (has text part after), shows "Thoughts" label
  await expect(page.getByTestId("thinking-card")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Here is my answer.")).toBeVisible();
});

test("thinking content is hidden by default inside details", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-2",
    Role: "assistant",
    Parts: [
      { type: "thinking", Thinking: "Secret reasoning content" },
      { type: "text", Text: "Visible answer." },
    ],
  }));

  // Thinking card visible but content hidden (inside closed details)
  await expect(page.getByTestId("thinking-card")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).not.toBeVisible();
});

test("clicking thinking toggle reveals content", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-3",
    Role: "assistant",
    Parts: [
      { type: "thinking", Thinking: "Revealed reasoning" },
      { type: "text", Text: "Answer." },
    ],
  }));

  await page.getByTestId("thinking-toggle").click();
  await expect(page.getByTestId("thinking-content")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).toContainText("Revealed reasoning");
});

// ── Tool call ──────────────────────────────────────────────────────────

test("tool call shows running indicator when not finished", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-4",
    Role: "assistant",
    Parts: [
      { type: "tool_call", ID: "tc-1", Name: "read_file", Input: '{"path":"/tmp/test.txt"}', Finished: false },
    ],
  }));

  await expect(page.getByTestId("tool-call")).toContainText("read_file", { timeout: 2000 });
  await expect(page.getByTestId("tool-call-running")).toBeVisible();
});

test("tool call hides running indicator when finished", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-5",
    Role: "assistant",
    Parts: [
      { type: "tool_call", ID: "tc-2", Name: "bash", Input: '{"command":"ls"}', Finished: true },
    ],
  }));

  await expect(page.getByTestId("tool-call")).toContainText("bash", { timeout: 2000 });
  await expect(page.getByTestId("tool-call-running")).not.toBeVisible();
});

test("tool call input is formatted as JSON", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-6",
    Role: "assistant",
    Parts: [
      { type: "tool_call", ID: "tc-3", Name: "write_file", Input: '{"path":"/tmp/test.txt","content":"hello"}', Finished: true },
    ],
  }));

  // formatJSON should pretty-print the input
  await expect(page.getByTestId("tool-call")).toContainText('"path"', { timeout: 2000 });
  await expect(page.getByTestId("tool-call")).toContainText('"content"');
});

// ── Tool result ────────────────────────────────────────────────────────

test("tool result with IsError shows error badge", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-7",
    Role: "tool",
    Parts: [
      { type: "tool_result", ToolCallID: "tc-err", Name: "bash", Content: "command not found", IsError: true },
    ],
  }));

  await expect(page.getByTestId("tool-result")).toContainText("command not found", { timeout: 2000 });
  await expect(page.getByTestId("tool-result-error")).toBeVisible();
});

test("tool result without error has no error badge", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-8",
    Role: "tool",
    Parts: [
      { type: "tool_result", ToolCallID: "tc-ok", Name: "bash", Content: "success output", IsError: false },
    ],
  }));

  await expect(page.getByTestId("tool-result")).toContainText("success output", { timeout: 2000 });
  // No error badge
  await expect(page.getByTestId("tool-result-error")).not.toBeVisible({ timeout: 1000 });
});

// ── Finish part ────────────────────────────────────────────────────────

test("finish part renders nothing visible", async ({ page }) => {
  await setupWithMessage(page, makeMessage({
    ID: "tp-9",
    Role: "assistant",
    Parts: [
      { type: "text", Text: "Answer before finish" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  }));

  await expect(page.getByText("Answer before finish")).toBeVisible({ timeout: 2000 });
  // Finish part should not add any visible content
  await expect(page.getByText("end_turn")).not.toBeVisible();
});
