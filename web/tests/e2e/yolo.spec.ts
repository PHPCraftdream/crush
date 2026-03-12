/**
 * YOLO E2E Test - Полный цикл с реальным бэкендом
 *
 * 1. Нажать YOLO → флаг сохраняется на бэкенде
 * 2. Переключиться на другую сессию → YOLO выключен
 * 3. Вернуться назад → YOLO восстановлен из бэкенда
 */

import { test, expect } from "@playwright/test";

test("YOLO toggle persists and restores per session", async ({ page }) => {
  await page.goto("http://127.0.0.1:60984");
  await page.waitForLoadState("networkidle");

  // Создаем тестовую сессию
  const sess1ID = await createE2ESession(page, "yolo-test-1");
  await page.waitForTimeout(500);

  // Кликаем на сессию чтобы активировать
  await page.getByText("e2e-test-yolo-test-1").first().click();
  await page.waitForTimeout(500);

  // Проверяем что YOLO выключен (кнопка без желтого фона)
  const yoloBtn = page.locator("button").filter({ hasText: /^Yolo$/ });
  await expect(yoloBtn).not.toHaveClass(/bg-yellow/);

  console.log("[TEST] YOLO initially OFF");

  // Включаем YOLO
  await yoloBtn.click();
  await page.waitForTimeout(500);

  // Проверяем что кнопка загорелась
  await expect(yoloBtn).toHaveClass(/bg-yellow/);
  console.log("[TEST] YOLO turned ON - button highlighted");

  // Создаем вторую сессию
  const sess2ID = await createE2ESession(page, "yolo-test-2");
  await page.waitForTimeout(500);

  // Переключаемся на вторую сессию
  await page.getByText("e2e-test-yolo-test-2").first().click();
  await page.waitForTimeout(500);

  // YOLO должен быть выключен для новой сессии
  await expect(yoloBtn).not.toHaveClass(/bg-yellow/);
  console.log("[TEST] Session 2: YOLO is OFF");

  // Возвращаемся к первой сессии
  await page.getByText("e2e-test-yolo-test-1").first().click();
  await page.waitForTimeout(500);

  // YOLO должен восстановиться из бэкенда!
  await expect(yoloBtn).toHaveClass(/bg-yellow/);
  console.log("[TEST] Back to session 1: YOLO restored from backend!");

  // Cleanup
  await deleteE2ESession(page, sess1ID);
  await deleteE2ESession(page, sess2ID);
});

// ── Helpers ─────────────────────────────────────────────────────────────────

async function createE2ESession(page: any, title: string): Promise<string> {
  const msgID = crypto.randomUUID();
  const result = await page.evaluate(async ([title, msgID]) => {
    const ws = (window as any).__ws;
    if (!ws) return null;

    return new Promise((resolve) => {
      const unsub = ws.on("*", (msg: any) => {
        if (msg.id === msgID) {
          unsub();
          resolve(msg.payload?.ID || null);
        }
      });
      ws.send("create_session", { title }, msgID);
    });
  }, [title, msgID]);

  await page.waitForTimeout(300);
  return result as string;
}

async function deleteE2ESession(page: any, sessionID: string): Promise<void> {
  await page.evaluate(([id]) => {
    const ws = (window as any).__ws;
    ws?.send("delete_session", { sessionID: id });
  }, [sessionID]);
}
