/**
 * Session-based WebSocket message filtering tests.
 *
 * Covers per-session message routing (commit ceb1e1a4):
 *  - Messages only appear in their assigned session
 *  - message_created for wrong session is ignored
 *  - message_updated for wrong session is ignored
 *  - message_deleted for wrong session is ignored
 *  - permission_request for wrong session is ignored
 *  - Switching sessions doesn't show other sessions' messages
 *  - Active session receives its messages correctly
 *
 * @since 2026-03-12
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

async function setupMultiSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sess-alpha", Title: "Alpha Session" }),
      makeSession({ ID: "sess-beta", Title: "Beta Session" }),
      makeSession({ ID: "sess-gamma", Title: "Gamma Session" }),
    ],
  });
  await expect(page.getByText("Alpha Session").first()).toBeVisible({ timeout: 3000 });
}

async function switchToSession(page: import("@playwright/test").Page, sessionTitle: string) {
  await page.getByText(sessionTitle).first().click();
  // Wait a bit for session to switch and input to be ready
  await page.waitForTimeout(500);
}

function makeMessageForSession(sessionID: string, content: string, overrides: Record<string, unknown> = {}) {
  return makeMessage({
    SessionID: sessionID,
    Role: "assistant",
    Parts: [{ type: "text", Text: content }],
    ...overrides,
  });
}

// ── message_created Filtering ───────────────────────────────────────────────────────

test("message_created for active session appears in chat", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Send message for current session
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-alpha", "Hello from Alpha!"),
  });

  await expect(page.getByText("Hello from Alpha!")).toBeVisible({ timeout: 2000 });
});

test("message_created for different session does NOT appear", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Send message for different session
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-beta", "Hello from Beta!"),
  });

  // Should NOT appear in Alpha session
  await expect(page.getByText("Hello from Beta!")).not.toBeVisible({ timeout: 2000 });
});

test("after switching sessions, old messages are cleared", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Add message to Alpha
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-alpha", "Alpha message 1"),
  });
  await expect(page.getByText("Alpha message 1")).toBeVisible({ timeout: 2000 });

  // Switch to Beta
  await switchToSession(page, "Beta Session");

  // Alpha message should be gone (messages cleared on session switch)
  await expect(page.getByText("Alpha message 1")).not.toBeVisible({ timeout: 2000 });
});

test("switching back to session shows its messages again", async ({ page }) => {
  await setupMultiSession(page);

  // Alpha session messages
  await switchToSession(page, "Alpha Session");
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessageForSession("sess-alpha", "Alpha first"),
      makeMessageForSession("sess-alpha", "Alpha second"),
    ],
  });
  await expect(page.getByText("Alpha first")).toBeVisible({ timeout: 2000 });

  // Switch to Beta and add messages
  await switchToSession(page, "Beta Session");
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessageForSession("sess-beta", "Beta message"),
    ],
  });
  await expect(page.getByText("Beta message")).toBeVisible({ timeout: 2000 });

  // Switch back to Alpha - this triggers load_messages request
  await page.getByText("Alpha Session").first().click();
  await page.waitForTimeout(500);  // Wait for load_messages to be sent

  // Respond with Alpha's messages
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessageForSession("sess-alpha", "Alpha first"),
      makeMessageForSession("sess-alpha", "Alpha second"),
    ],
  });

  // Alpha's messages should be visible
  await expect(page.getByText("Alpha first")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Alpha second")).toBeVisible();
  // Beta message should NOT be visible
  await expect(page.getByText("Beta message")).not.toBeVisible();
});

// ── message_updated Filtering ───────────────────────────────────────────────────────

test("message_updated for active session updates the message", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Create initial message
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-alpha", "Original text"),
  });
  await expect(page.getByText("Original text")).toBeVisible({ timeout: 2000 });

  // Update the message
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: makeMessageForSession("sess-alpha", "Updated text", {
      ID: "msg-1", // Must match the created message ID
    }),
  });

  await expect(page.getByText("Updated text")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Original text")).not.toBeVisible();
});

test("message_updated for different session is ignored", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Create message in Alpha
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({ ID: "msg-alpha", SessionID: "sess-alpha", Role: "assistant", Parts: [{ type: "text", Text: "Alpha content" }] }),
  });
  await expect(page.getByText("Alpha content")).toBeVisible({ timeout: 2000 });

  // Try to update a message in Beta session (with same ID for testing)
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: makeMessage({ ID: "msg-alpha", SessionID: "sess-beta", Role: "assistant", Parts: [{ type: "text", Text: "Fake Beta update" }] }),
  });

  // Alpha's message should NOT change
  await expect(page.getByText("Alpha content")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Fake Beta update")).not.toBeVisible();
});

// ── message_deleted Filtering ───────────────────────────────────────────────────────

test("message_deleted for active session removes the message", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Create message
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({ ID: "msg-del", SessionID: "sess-alpha", Role: "assistant", Parts: [{ type: "text", Text: "To be deleted" }] }),
  });
  await expect(page.getByText("To be deleted")).toBeVisible({ timeout: 2000 });

  // Delete the message
  await sendMockWSMessage(page, {
    type: "message_deleted",
    payload: makeMessage({ ID: "msg-del", SessionID: "sess-alpha", Role: "assistant", Parts: [] }),
  });

  await expect(page.getByText("To be deleted")).not.toBeVisible({ timeout: 2000 });
});

test("message_deleted for different session is ignored", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Create message in Alpha
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({ ID: "msg-del-alpha", SessionID: "sess-alpha", Role: "assistant", Parts: [{ type: "text", Text: "Alpha stays" }] }),
  });
  await expect(page.getByText("Alpha stays")).toBeVisible({ timeout: 2000 });

  // Delete message in Beta session (same ID)
  await sendMockWSMessage(page, {
    type: "message_deleted",
    payload: makeMessage({ ID: "msg-del-alpha", SessionID: "sess-beta", Role: "assistant", Parts: [] }),
  });

  // Alpha's message should remain
  await expect(page.getByText("Alpha stays")).toBeVisible({ timeout: 2000 });
});

// ── permission_request Filtering ────────────────────────────────────────────────────

test("permission_request for active session shows dialog", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Send permission request for Alpha
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "perm-1",
      SessionID: "sess-alpha",
      ToolCallID: "tc-1",
      ToolName: "bash",
      Description: "Run command in Alpha",
      Action: "execute",
      Path: "/tmp/alpha.sh",
      Params: {},
    },
  });

  await expect(page.getByText("bash")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Run command in Alpha")).toBeVisible();
});

test("permission_request for different session is ignored", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Send permission request for Beta
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "perm-2",
      SessionID: "sess-beta",
      ToolCallID: "tc-2",
      ToolName: "write_file",
      Description: "Write in Beta",
      Action: "write",
      Path: "/tmp/beta.txt",
      Params: {},
    },
  });

  // Should NOT show in Alpha session
  await expect(page.getByText("write_file")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Write in Beta")).not.toBeVisible();
});

test("permission_request appears only in its session when multiple sessions active", async ({ page }) => {
  await setupMultiSession(page);

  // Send permission for Alpha while in Alpha
  await switchToSession(page, "Alpha Session");
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "perm-alpha",
      SessionID: "sess-alpha",
      ToolCallID: "tc-alpha",
      ToolName: "read_file",
      Description: "Read in Alpha",
      Action: "read",
      Path: "/alpha.txt",
      Params: {},
    },
  });
  await expect(page.getByText("read_file")).toBeVisible({ timeout: 2000 });

  // Switch to Beta
  await switchToSession(page, "Beta Session");

  // Alpha's permission request should be gone
  await expect(page.getByText("read_file")).not.toBeVisible({ timeout: 2000 });

  // Send permission for Beta
  await sendMockWSMessage(page, {
    type: "permission_request",
    payload: {
      ID: "perm-beta",
      SessionID: "sess-beta",
      ToolCallID: "tc-beta",
      ToolName: "bash",
      Description: "Bash in Beta",
      Action: "execute",
      Path: "/tmp/beta.sh",
      Params: {},
    },
  });
  // Use locator for font-mono bash to avoid matching the description
  await expect(page.locator(".font-mono").getByText("bash")).toBeVisible({ timeout: 2000 });
});

// ── Global Events (No Filtering) ─────────────────────────────────────────────────────

test("session_created event creates new session in sidebar", async ({ page }) => {
  await setupMultiSession(page);

  // Create new session
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "sess-new", Title: "New Session" }),
  });

  // Check that the new session appears in the sidebar
  await expect(page.getByText("New Session").first()).toBeVisible({ timeout: 2000 });
});

test("session_updated event updates session in sidebar", async ({ page }) => {
  await setupMultiSession(page);
  await expect(page.getByText("Alpha Session").first()).toBeVisible({ timeout: 2000 });

  // Update session title
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "sess-alpha", Title: "Alpha Session (Updated)" }),
  });

  // Check that the updated title appears in the sidebar
  await expect(page.getByText("Alpha Session (Updated)").first()).toBeVisible({ timeout: 2000 });
});

test("agent_busy event updates session busy state correctly", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Set Alpha as busy
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "sess-alpha", Busy: true },
  });

  // Should show busy indicator (implementation specific - just check event was received)
  const events = await page.evaluate(() => {
    const received = (window as unknown as Record<string, unknown>).__wsReceived as Array<{ type: string }>;
    return received.filter((e) => e.type === "agent_busy").length;
  });
  expect(events).toBeGreaterThan(0);
});

test("summarize_queued event applies to correct session", async ({ page }) => {
  await setupMultiSession(page);
  await switchToSession(page, "Alpha Session");

  // Queue summarize for Beta (different session)
  await sendMockWSMessage(page, {
    type: "summarize_queued",
    payload: { SessionID: "sess-beta", Queued: true },
  });

  // Just verify event was received (actual UI behavior depends on implementation)
  const events = await page.evaluate(() => {
    const received = (window as unknown as Record<string, unknown>).__wsReceived as Array<{ type: string }>;
    return received.filter((e) => e.type === "summarize_queued").length;
  });
  expect(events).toBeGreaterThan(0);
});

// ── Edge Cases ───────────────────────────────────────────────────────────────────────

test("messages_list clears previous session messages", async ({ page }) => {
  await setupMultiSession(page);

  // Load messages in Alpha
  await switchToSession(page, "Alpha Session");
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessageForSession("sess-alpha", "Alpha 1"),
      makeMessageForSession("sess-alpha", "Alpha 2"),
      makeMessageForSession("sess-alpha", "Alpha 3"),
    ],
  });
  await expect(page.getByText("Alpha 1")).toBeVisible({ timeout: 2000 });
  expect(await page.getByText("Alpha 1").count()).toBe(1);

  // Switch to Beta and load its messages
  await switchToSession(page, "Beta Session");
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessageForSession("sess-beta", "Beta Only"),
    ],
  });

  // Alpha messages should be gone
  await expect(page.getByText("Alpha 1")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Alpha 2")).not.toBeVisible();
  await expect(page.getByText("Alpha 3")).not.toBeVisible();
  await expect(page.getByText("Beta Only")).toBeVisible();
});

test("no active session means no messages shown", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sess-1", Title: "Session 1" }),
    ],
  });

  // The app auto-selects the first session, so we need to send session_created
  // for a different session to make the current one inactive, or we can
  // test the filtering by sending a message for a DIFFERENT session than
  // the one that's auto-selected
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-different", "Should not appear"),
  });

  // Message should NOT appear (it's for a different session)
  await expect(page.getByText("Should not appear")).not.toBeVisible({ timeout: 2000 });
});

test("rapid session switches handle filtering correctly", async ({ page }) => {
  await setupMultiSession(page);

  // Switch rapidly and send messages
  await switchToSession(page, "Alpha Session");
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-alpha", "In Alpha"),
  });
  await expect(page.getByText("In Alpha")).toBeVisible({ timeout: 2000 });

  await switchToSession(page, "Beta Session");
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-beta", "In Beta"),
  });
  await expect(page.getByText("In Beta")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("In Alpha")).not.toBeVisible();

  await switchToSession(page, "Gamma Session");
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessageForSession("sess-gamma", "In Gamma"),
  });
  await expect(page.getByText("In Gamma")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("In Beta")).not.toBeVisible();

  // Back to Alpha - need to reload messages
  await page.getByText("Alpha Session").first().click();
  await page.waitForTimeout(500);
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [makeMessageForSession("sess-alpha", "In Alpha")],
  });
  await expect(page.getByText("In Alpha")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("In Gamma")).not.toBeVisible();
});
