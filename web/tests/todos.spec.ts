/**
 * Todos (task list) integration tests.
 *
 * Verifies:
 *  1. TodoList panel appears when a session has todos.
 *  2. Panel is hidden when session has no todos.
 *  3. Status cycle: pending → in_progress → completed → pending.
 *  4. Inline edit on double-click; Enter commits, Escape cancels.
 *  5. Delete todo sends update_todos without the deleted item.
 *  6. Move up / move down reorders todos and sends update_todos.
 *  7. Collapse / expand toggle works.
 *  8. session_updated event with new todos updates the panel.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

const TODO_PENDING    = { content: "Write tests", status: "pending" };
const TODO_IN_PROG    = { content: "Fix bug", status: "in_progress" };
const TODO_DONE       = { content: "Deploy", status: "completed" };

async function setup(page: import("@playwright/test").Page, todos: unknown[] = [TODO_PENDING, TODO_IN_PROG, TODO_DONE]) {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "td-1", Title: "Todo Session", Todos: todos })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Todo Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Todo Session").first().click();
}

// ── 1. Panel visible when todos exist ────────────────────────────────────────

test("todo panel visible when session has todos", async ({ page }) => {
  await setup(page);
  await expect(page.getByTestId("todo-list")).toBeVisible({ timeout: 3000 });
});

// ── 2. Panel hidden when no todos ────────────────────────────────────────────

test("todo panel hidden when session has no todos", async ({ page }) => {
  await setup(page, []);
  await expect(page.getByTestId("todo-list")).not.toBeAttached({ timeout: 2000 });
});

// ── 3. Count badge shows completed/total ─────────────────────────────────────

test("task counter shows completed/total", async ({ page }) => {
  await setup(page);
  // 1 completed out of 3
  await expect(page.getByTestId("todo-list")).toContainText("1/3", { timeout: 3000 });
});

// ── 4. Status cycle ───────────────────────────────────────────────────────────

test("clicking status button cycles pending→in_progress→completed and sends update_todos", async ({ page }) => {
  await setup(page, [{ content: "Task A", status: "pending" }]);

  const statusBtn = page.getByTestId("todo-status-btn").first();
  await statusBtn.click();

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { sessionID: string; todos: Array<{ status: string }> };
  expect(p.sessionID).toBe("td-1");
  expect(p.todos[0].status).toBe("in_progress");
});

test("status cycles in_progress→completed on second click", async ({ page }) => {
  await setup(page, [{ content: "Task B", status: "in_progress" }]);

  await page.getByTestId("todo-status-btn").first().click();

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { todos: Array<{ status: string }> };
  expect(p.todos[0].status).toBe("completed");
});

test("status cycles completed→pending on third click", async ({ page }) => {
  await setup(page, [{ content: "Task C", status: "completed" }]);

  await page.getByTestId("todo-status-btn").first().click();

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { todos: Array<{ status: string }> };
  expect(p.todos[0].status).toBe("pending");
});

// ── 5. Inline edit ────────────────────────────────────────────────────────────

test("pencil button opens edit mode, Enter commits and sends update_todos", async ({ page }) => {
  await setup(page, [{ content: "Original text", status: "pending" }]);

  await page.getByTestId("todo-row").first().hover();
  await page.getByTestId("todo-edit").first().click();
  await page.getByTestId("todo-edit-input").fill("Updated text");
  await page.getByTestId("todo-edit-input").press("Enter");

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { todos: Array<{ content: string }> };
  expect(p.todos[0].content).toBe("Updated text");
});

test("save button (✓) commits edit and sends update_todos", async ({ page }) => {
  await setup(page, [{ content: "Save via button", status: "pending" }]);

  await page.getByTestId("todo-row").first().hover();
  await page.getByTestId("todo-edit").first().click();
  await page.getByTestId("todo-edit-input").fill("Saved content");
  await page.getByTestId("todo-save").click();

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { todos: Array<{ content: string }> };
  expect(p.todos[0].content).toBe("Saved content");
});

test("Escape cancels edit without sending update_todos", async ({ page }) => {
  await setup(page, [{ content: "Keep this", status: "pending" }]);

  await page.getByTestId("todo-row").first().hover();
  await page.getByTestId("todo-edit").first().click();
  await page.getByTestId("todo-edit-input").fill("Changed");
  await page.getByTestId("todo-edit-input").press("Escape");

  // Content should revert
  await expect(page.getByTestId("todo-content").first()).toHaveText("Keep this", { timeout: 2000 });
  // No update_todos should have been sent
  const sent = await page.evaluate(() =>
    ((window as unknown) as Record<string, unknown[]>)["__wsSent"]
      .filter((m: unknown) => (m as { type: string }).type === "update_todos")
  );
  expect(sent).toHaveLength(0);
});

// ── 6. Delete ─────────────────────────────────────────────────────────────────

test("delete button removes todo and sends update_todos without it", async ({ page }) => {
  await setup(page, [
    { content: "Keep me", status: "pending" },
    { content: "Delete me", status: "pending" },
  ]);

  // Hover the second row to reveal delete button
  const rows = page.getByTestId("todo-row");
  await rows.nth(1).hover();
  await page.getByTestId("todo-delete").nth(1).click();

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { todos: Array<{ content: string }> };
  expect(p.todos).toHaveLength(1);
  expect(p.todos[0].content).toBe("Keep me");
});

// ── 7. Move up / move down ────────────────────────────────────────────────────

test("move down swaps todo with the next one and sends update_todos", async ({ page }) => {
  await setup(page, [
    { content: "First", status: "pending" },
    { content: "Second", status: "pending" },
  ]);

  const rows = page.getByTestId("todo-row");
  await rows.first().hover();
  await page.getByTestId("todo-move-down").first().click();

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { todos: Array<{ content: string }> };
  expect(p.todos[0].content).toBe("Second");
  expect(p.todos[1].content).toBe("First");
});

test("move up swaps todo with the previous one and sends update_todos", async ({ page }) => {
  await setup(page, [
    { content: "Alpha", status: "pending" },
    { content: "Beta", status: "pending" },
  ]);

  const rows = page.getByTestId("todo-row");
  await rows.nth(1).hover();
  await page.getByTestId("todo-move-up").nth(1).click();

  const cmd = await waitForWSSend(page, "update_todos");
  const p = cmd.payload as { todos: Array<{ content: string }> };
  expect(p.todos[0].content).toBe("Beta");
  expect(p.todos[1].content).toBe("Alpha");
});

test("move-up disabled for first item, move-down disabled for last", async ({ page }) => {
  await setup(page, [
    { content: "Only one", status: "pending" },
  ]);

  const row = page.getByTestId("todo-row").first();
  await row.hover();
  await expect(page.getByTestId("todo-move-up").first()).toBeDisabled({ timeout: 2000 });
  await expect(page.getByTestId("todo-move-down").first()).toBeDisabled({ timeout: 2000 });
});

// ── 8. Collapse / expand ──────────────────────────────────────────────────────

test("clicking header collapses and re-expands the todo list", async ({ page }) => {
  await setup(page);

  const toggle = page.getByTestId("todo-list-toggle");
  const rows = page.getByTestId("todo-row");

  // Initially expanded
  await expect(rows.first()).toBeVisible({ timeout: 2000 });

  // Collapse
  await toggle.click();
  await expect(rows.first()).not.toBeVisible({ timeout: 2000 });

  // Expand again
  await toggle.click();
  await expect(rows.first()).toBeVisible({ timeout: 2000 });
});

// ── 9. session_updated refreshes todos ───────────────────────────────────────

test("session_updated event with new todos updates the displayed list", async ({ page }) => {
  await setup(page, [{ content: "Old task", status: "pending" }]);
  await expect(page.getByTestId("todo-content").first()).toHaveText("Old task", { timeout: 3000 });

  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({
      ID: "td-1",
      Title: "Todo Session",
      Todos: [
        { content: "New task A", status: "in_progress" },
        { content: "New task B", status: "completed" },
      ],
    }),
  });

  await expect(page.getByTestId("todo-content").first()).toHaveText("New task A", { timeout: 3000 });
  await expect(page.getByTestId("todo-list")).toContainText("1/2");
});
