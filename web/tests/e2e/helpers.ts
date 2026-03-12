import { Page } from '@playwright/test';

/**
 * E2E Test Helpers
 *
 * These helpers manage real backend sessions for testing.
 * All test sessions are prefixed with "e2e-test-" to avoid conflicts.
 */

const E2E_PREFIX = "e2e-test-";

/**
 * Clean up all e2e test sessions from the backend
 */
export async function cleanupE2ESessions(page: Page) {
  console.log("[E2E] Cleaning up test sessions...");

  // Get all sessions
  const sessions = await page.evaluate(() => {
    return (window as unknown as Record<string, unknown>)["__wsReceived"] as Array<{
      type: string;
      payload: { sessions: Array<{ ID: string }> };
    }> | [];
  }) || [];

  const sessionsList = sessions
    .filter(s => s.type === "sessions_list")
    .flatMap(s => s.payload?.sessions || [])
    .filter(s => s.Title?.startsWith(E2E_PREFIX));

  for (const session of sessionsList) {
    await page.evaluate((id: string) => {
      const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket);
      if (ws && ws.readyState === 1) {
        ws.send(JSON.stringify({ type: "delete_session", payload: { sessionID: id } }));
      }
    }, session.ID);
  }

  // Wait a bit for deletion
  await page.waitForTimeout(500);
}

/**
 * Create a test session with a unique name
 */
export async function createE2ESession(page: Page, title: string): Promise<string> {
  const sessionID = await page.evaluate((t: string) => {
    const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket;
    if (!ws || ws.readyState !== 1) {
      throw new Error("WebSocket not connected");
    }
    ws.send(JSON.stringify({ type: "create_session", payload: { title: t } }));
    return null; // Session ID will come from session_created event
  }, `${E2E_PREFIX}${title}`);

  // Wait for session_created event
  await page.waitForTimeout(1000);

  // Find the session ID from the sessions list
  const sessions = await page.evaluate(() => {
    return ((window as unknown) as Record<string, unknown>)["__wsReceived"] as Array<{
      type: string;
      payload: { sessions: Array<{ ID: string; Title: string }> };
    }>;
  }) || [];

  const session = sessions
    .find(s => s.type === "sessions_list" || s.type === "session_created")
    ?.payload?.sessions?.find((s: { Title: string }) => s.Title?.startsWith(E2E_PREFIX + title));

  if (!session) {
    throw new Error(`Failed to create session: ${E2E_PREFIX}${title}`);
  }

  console.log("[E2E] Created session:", session.ID, session.Title);
  return session.ID;
}

/**
 * Delete a test session
 */
export async function deleteE2ESession(page: Page, sessionID: string): Promise<void> {
  await page.evaluate((id: string) => {
    const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket;
    if (ws && ws.readyState === 1) {
      ws.send(JSON.stringify({ type: "delete_session", payload: { sessionID: id } }));
    }
  }, sessionID);
  await page.waitForTimeout(500);
  console.log("[E2E] Deleted session:", sessionID);
}

/**
 * Send a message to the AI and wait for response
 */
export async function sendMessage(page: Page, sessionID: string, content: string): Promise<void> {
  await page.evaluate(({ sid, msg }: { sid: string; msg: string }) => {
    const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket;
    if (ws && ws.readyState === 1) {
      ws.send(JSON.stringify({ type: "send_message", payload: { sessionID: sid, content: msg } }));
    }
  }, { sessionID, content });
  console.log("[E2E] Sent message:", content);
}

/**
 * Switch to a session by clicking on it
 */
export async function switchToSession(page: Page, title: string): Promise<void> {
  await page.getByText(`${E2E_PREFIX}${title}`).first().click();
  await page.waitForTimeout(500);
  console.log("[E2E] Switched to session:", title);
}

/**
 * Open permissions modal
 */
export async function openPermissionsModal(page: Page): Promise<void> {
  const yoloBtn = page.locator("button.btn-toolbar").filter({ hasText: /^Yolo$/ });
  await yoloBtn.click();
  await page.expect(page.getByText("YOLO Mode")).toBeVisible({ timeout: 3000 });
  console.log("[E2E] Opened permissions modal");
}
