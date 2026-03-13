import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Listing ────────────────────────────────────────────────────────────────

test("session list appears after connect", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "s1", Title: "Alpha Session" })],
  });
  await expect(page.getByTestId("session-title-s1")).toBeVisible({ timeout: 3000 });
});

test("shows message count in sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "s2", Title: "My Chat", MessageCount: 5 })],
  });
  await expect(page.getByTestId("session-msg-count-s2")).toHaveText("5 msgs", { timeout: 2000 });
});

test("shows singular msg for count of 1", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "s-one", Title: "One Msg", MessageCount: 1 })],
  });
  await expect(page.getByTestId("session-msg-count-s-one")).toHaveText("1 msg", { timeout: 2000 });
});

test("shows token usage in sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({
        ID: "s3",
        Title: "Token Session",
        PromptTokens: 1500,
        CompletionTokens: 500,
      }),
    ],
  });
  // Only check the sidebar span (session not selected, so header won't show it)
  await expect(
    page.getByTestId("session-tokens-s3")
  ).toHaveText("2.0k tok", { timeout: 2000 });
});

test("shows no sessions message when list is empty", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await expect(page.getByTestId("sidebar-empty")).toBeVisible({ timeout: 2000 });
});

test("multiple sessions all appear", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "s-a", Title: "Session A" }),
      makeSession({ ID: "s-b", Title: "Session B" }),
      makeSession({ ID: "s-c", Title: "Session C" }),
    ],
  });
  await expect(page.getByTestId("session-title-s-a")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-title-s-b")).toBeVisible();
  await expect(page.getByTestId("session-title-s-c")).toBeVisible();
});

// ── Session events ─────────────────────────────────────────────────────────

test("session_created event adds session to sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "new-1", Title: "Brand New Session" }),
  });
  await expect(page.getByTestId("session-title-new-1")).toBeVisible({ timeout: 2000 });
});

test("session_updated event updates session title", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "upd-1", Title: "Old Title" })],
  });
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "upd-1", Title: "New Title" }),
  });
  await expect(page.getByTestId("session-title-upd-1")).toHaveText("New Title", { timeout: 2000 });
});

test("session_deleted event removes session from sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "del-1", Title: "Delete Me" }),
      makeSession({ ID: "del-2", Title: "Keep Me" }),
    ],
  });
  await expect(page.getByTestId("session-del-1")).toBeVisible({ timeout: 2000 });
  await sendMockWSMessage(page, {
    type: "session_deleted",
    payload: { ID: "del-1" },
  });
  await expect(page.getByTestId("session-del-1")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-del-2")).toBeVisible();
});

// ── Selection ──────────────────────────────────────────────────────────────

test("selecting a session shows empty chat placeholder", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sel-1", Title: "Empty Session" })],
  });
  await expect(page.getByTestId("session-title-sel-1")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sel-1").click();
  await expect(page.getByText("No messages yet")).toBeVisible({ timeout: 2000 });
});

test("selecting a session shows its messages from messages_list", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sel-2", Title: "Has Messages" })],
  });
  await expect(page.getByTestId("session-title-sel-2")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sel-2").click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "m1",
        SessionID: "sel-2",
        Role: "user",
        Parts: [{ type: "text", Text: "First user message" }],
      }),
      makeMessage({
        ID: "m2",
        SessionID: "sel-2",
        Role: "assistant",
        Parts: [{ type: "text", Text: "First assistant reply" }],
      }),
    ],
  });
  await expect(page.getByText("First user message")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("First assistant reply")).toBeVisible();
});

test("switching sessions clears and reloads messages", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sw-1", Title: "Session One" }),
      makeSession({ ID: "sw-2", Title: "Session Two" }),
    ],
  });

  // Select first, load messages
  await expect(page.getByTestId("session-title-sw-1")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sw-1").click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({ ID: "m-a", SessionID: "sw-1", Parts: [{ type: "text", Text: "Message from One" }] }),
    ],
  });
  await expect(page.getByText("Message from One")).toBeVisible({ timeout: 2000 });

  // Switch to second, different messages appear
  await page.getByTestId("session-sw-2").click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({ ID: "m-b", SessionID: "sw-2", Parts: [{ type: "text", Text: "Message from Two" }] }),
    ],
  });
  await expect(page.getByText("Message from Two")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Message from One")).not.toBeVisible();
});

test("selecting a session updates the header title", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "hdr-1", Title: "Header Test Session" })],
  });
  await expect(page.getByTestId("session-title-hdr-1")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-hdr-1").click();
  await expect(
    page.locator("header h1").getByText("Header Test Session")
  ).toBeVisible({ timeout: 2000 });
});

// ── Session creation ────────────────────────────────────────────────────────

test("clicking + sends create_session command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await page.getByTestId("sidebar-new-session").click();
  const cmd = await waitForWSSend(page, "create_session");
  expect(cmd.type).toBe("create_session");
});

test("session_created event adds session after + click", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await page.getByTestId("sidebar-new-session").click();
  await waitForWSSend(page, "create_session");
  // Server responds with session_created (now correctly broadcast by Go server)
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "cr-1", Title: "New Session" }),
  });
  await expect(page.getByTestId("session-title-cr-1")).toBeVisible({ timeout: 2000 });
});

test("multiple creates accumulate in sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await page.getByTestId("sidebar-new-session").click();
  await waitForWSSend(page, "create_session");
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "cr-a", Title: "Session Alpha" }),
  });
  await page.getByTestId("sidebar-new-session").click();
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "cr-b", Title: "Session Beta" }),
  });
  await expect(page.getByTestId("session-title-cr-a")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-title-cr-b")).toBeVisible();
});

// ── Session deletion ────────────────────────────────────────────────────────

test("delete button sends delete_session command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "del-x", Title: "To Delete" })],
  });
  await expect(page.getByTestId("session-title-del-x")).toBeVisible({ timeout: 3000 });
  // Hover the session row to reveal delete button
  const sessionRow = page.getByTestId("session-del-x");
  await sessionRow.hover();
  await page.getByTestId("session-delete-del-x").click();
  // Confirm the delete dialog
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  const cmd = await waitForWSSend(page, "delete_session");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("del-x");
});

test("deleted session disappears from sidebar immediately", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "del-keep", Title: "Keep This" }),
      makeSession({ ID: "del-gone", Title: "Delete This" }),
    ],
  });
  await expect(page.getByTestId("session-title-del-gone")).toBeVisible({ timeout: 3000 });
  // Hover and click delete
  const sessionRow = page.getByTestId("session-del-gone");
  await sessionRow.hover();
  await page.getByTestId("session-delete-del-gone").click();
  // Confirm the delete dialog
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await expect(page.getByTestId("session-del-gone")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-del-keep")).toBeVisible();
});

// ── Session rename ──────────────────────────────────────────────────────────

test("clicking header title opens rename input", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ren-1", Title: "Old Name" })],
  });
  await expect(page.getByTestId("session-title-ren-1")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-ren-1").click();
  // After selecting, click the header title to start rename
  await page.locator("header button[title='Click to rename']").click();
  await expect(page.getByTestId("session-edit-input")).toBeVisible({ timeout: 2000 });
});

test("renaming session sends rename_session command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ren-2", Title: "Original Title" })],
  });
  await expect(page.getByTestId("session-title-ren-2")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-ren-2").click();
  await page.locator("header button[title='Click to rename']").click();
  await page.getByTestId("session-edit-input").fill("Renamed Title");
  await page.getByTestId("session-edit-input").press("Enter");
  const cmd = await waitForWSSend(page, "rename_session");
  expect((cmd.payload as { sessionID: string; title: string }).sessionID).toBe("ren-2");
  expect((cmd.payload as { sessionID: string; title: string }).title).toBe("Renamed Title");
});

test("pressing Escape cancels rename without sending command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ren-3", Title: "Stay The Same" })],
  });
  await expect(page.getByTestId("session-title-ren-3")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-ren-3").click();
  await page.locator("header button[title='Click to rename']").click();
  await page.getByTestId("session-edit-input").fill("Attempted Change");
  await page.getByTestId("session-edit-input").press("Escape");
  // Input should close
  await expect(page.getByTestId("session-edit-input")).not.toBeVisible({ timeout: 2000 });
  // Title remains in button
  await expect(page.locator("header button[title='Click to rename']")).toBeVisible({ timeout: 2000 });
});

test("session_updated event updates title in header", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ren-4", Title: "Before Update" })],
  });
  await expect(page.getByTestId("session-title-ren-4")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-ren-4").click();
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "ren-4", Title: "After Update" }),
  });
  await expect(
    page.locator("header h1").getByText("After Update")
  ).toBeVisible({ timeout: 2000 });
});

// ── Summarization indicator ──────────────────────────────────────────────────

test("header shows summarized indicator when session has SummaryMessageID", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sum-1", Title: "Summarized Session", SummaryMessageID: "msg-sum-1", PromptTokens: 5000, CompletionTokens: 1000 })],
  });
  await expect(page.getByTestId("session-title-sum-1")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sum-1").click();
  await expect(page.getByTitle("Session has been summarized")).toBeVisible({ timeout: 2000 });
});
