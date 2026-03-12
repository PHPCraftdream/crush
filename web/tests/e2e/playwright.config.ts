import { defineConfig, devices } from '@playwright/test';

/**
 * E2E Test Configuration
 *
 * These tests run against a real backend and use real browser sessions.
 * To avoid interfering with development work:
 * - Tests create their own test sessions (prefixed with "e2e-test-")
 * - Tests clean up after themselves
 * - Only run these tests explicitly with `npm run test:e2e`
 *
 * IMPORTANT: Do NOT run these tests while working - they will create
 * sessions and send messages to your AI assistant!
 */
export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,  // Run sequentially to avoid session conflicts
  retries: 0,
  timeout: 30000,

  use: {
    baseURL: 'http://127.0.0.1:60984',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        // Run in headed mode for debugging, uncomment for CI:
        // headless: true,
      },
    },
  ],
});
