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
  await expect(page.getByText("Alpha Session")).toBeVisible({ timeout: 3000 });
});

test("shows message count in sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "s2", Title: "My Chat", MessageCount: 5 })],
  });
  await expect(page.getByText("5 msgs")).toBeVisible({ timeout: 2000 });
});

test("shows singular msg for count of 1", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "s-one", Title: "One Msg", MessageCount: 1 })],
  });
  await expect(page.getByText("1 msg")).toBeVisible({ timeout: 2000 });
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
    page.locator("aside").getByText("2.0k tok")
  ).toBeVisible({ timeout: 2000 });
});

test("shows no sessions message when list is empty", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await expect(page.getByText("No sessions yet")).toBeVisible({ timeout: 2000 });
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
  await expect(page.getByText("Session A")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Session B")).toBeVisible();
  await expect(page.getByText("Session C")).toBeVisible();
});

// ── Session events ─────────────────────────────────────────────────────────

test("session_created event adds session to sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "new-1", Title: "Brand New Session" }),
  });
  await expect(page.getByText("Brand New Session")).toBeVisible({ timeout: 2000 });
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
  await expect(page.getByText("New Title")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Old Title")).not.toBeVisible();
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
  await expect(page.getByText("Delete Me")).toBeVisible({ timeout: 2000 });
  await sendMockWSMessage(page, {
    type: "session_deleted",
    payload: { ID: "del-1" },
  });
  await expect(page.getByText("Delete Me")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Keep Me")).toBeVisible();
});

// ── Selection ──────────────────────────────────────────────────────────────

test("selecting a session shows empty chat placeholder", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sel-1", Title: "Empty Session" })],
  });
  await expect(page.getByText("Empty Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("Empty Session").click();
  await expect(page.getByText("No messages yet")).toBeVisible({ timeout: 2000 });
});

test("selecting a session shows its messages from messages_list", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sel-2", Title: "Has Messages" })],
  });
  await expect(page.getByText("Has Messages")).toBeVisible({ timeout: 3000 });
  await page.getByText("Has Messages").click();
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
  await expect(page.getByText("Session One")).toBeVisible({ timeout: 3000 });
  await page.getByText("Session One").click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({ ID: "m-a", SessionID: "sw-1", Parts: [{ type: "text", Text: "Message from One" }] }),
    ],
  });
  await expect(page.getByText("Message from One")).toBeVisible({ timeout: 2000 });

  // Switch to second, different messages appear
  await page.getByText("Session Two").click();
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
  await expect(page.getByText("Header Test Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("Header Test Session").click();
  await expect(
    page.locator("header h1").getByText("Header Test Session")
  ).toBeVisible({ timeout: 2000 });
});

// ── Session creation ────────────────────────────────────────────────────────

test("clicking + sends create_session command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await page.getByTitle("New session").click();
  const cmd = await waitForWSSend(page, "create_session");
  expect(cmd.type).toBe("create_session");
});

test("session_created event adds session after + click", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await page.getByTitle("New session").click();
  await waitForWSSend(page, "create_session");
  // Server responds with session_created (now correctly broadcast by Go server)
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "cr-1", Title: "New Session" }),
  });
  await expect(page.getByText("New Session")).toBeVisible({ timeout: 2000 });
});

test("multiple creates accumulate in sidebar", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [] });
  await page.getByTitle("New session").click();
  await waitForWSSend(page, "create_session");
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "cr-a", Title: "Session Alpha" }),
  });
  await page.getByTitle("New session").click();
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "cr-b", Title: "Session Beta" }),
  });
  await expect(page.getByText("Session Alpha")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Session Beta")).toBeVisible();
});

// ── Session deletion ────────────────────────────────────────────────────────

test("delete button sends delete_session command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "del-x", Title: "To Delete" })],
  });
  await expect(page.getByText("To Delete")).toBeVisible({ timeout: 3000 });
  // Hover the session row to reveal delete button, then scope click to that row
  // The text lives two divs inside the session row (.group div)
  const row = page.locator("aside").getByText("To Delete").locator("../..");
  await row.hover();
  await row.getByTitle("Delete session").click();
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
  await expect(page.getByText("Delete This")).toBeVisible({ timeout: 3000 });
  // Scope delete button to the specific session row to avoid strict-mode violation
  const row = page.locator("aside").getByText("Delete This").locator("../..");
  await row.hover();
  await row.getByTitle("Delete session").click();
  await expect(page.getByText("Delete This")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Keep This")).toBeVisible();
});

// ── Session rename ──────────────────────────────────────────────────────────

test("clicking header title opens rename input", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ren-1", Title: "Old Name" })],
  });
  await expect(page.getByText("Old Name")).toBeVisible({ timeout: 3000 });
  await page.getByText("Old Name").first().click();
  // After selecting, click the header title to start rename
  await page.locator("header button[title='Click to rename']").click();
  await expect(page.locator("header input")).toBeVisible({ timeout: 2000 });
});

test("renaming session sends rename_session command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ren-2", Title: "Original Title" })],
  });
  await expect(page.getByText("Original Title")).toBeVisible({ timeout: 3000 });
  await page.getByText("Original Title").first().click();
  await page.locator("header button[title='Click to rename']").click();
  await page.locator("header input").fill("Renamed Title");
  await page.locator("header input").press("Enter");
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
  await expect(page.getByText("Stay The Same")).toBeVisible({ timeout: 3000 });
  await page.getByText("Stay The Same").first().click();
  await page.locator("header button[title='Click to rename']").click();
  await page.locator("header input").fill("Attempted Change");
  await page.locator("header input").press("Escape");
  // Input should close
  await expect(page.locator("header input")).not.toBeVisible({ timeout: 2000 });
  // Title remains in button
  await expect(page.locator("header button[title='Click to rename']")).toBeVisible({ timeout: 2000 });
});

test("session_updated event updates title in header", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ren-4", Title: "Before Update" })],
  });
  await expect(page.getByText("Before Update")).toBeVisible({ timeout: 3000 });
  await page.getByText("Before Update").first().click();
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
  await expect(page.getByText("Summarized Session")).toBeVisible({ timeout: 3000 });
  await page.getByText("Summarized Session").click();
  await expect(page.getByTitle("Session has been summarized")).toBeVisible({ timeout: 2000 });
});
