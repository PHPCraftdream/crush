import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  workers: "50%",
  use: {
    baseURL: "http://localhost:3000",
    // Recognize both data-testid and data-test-id attributes
    testIdAttribute: "data-test-id",
  },
  webServer: {
    command: "npm run dev",
    url: "http://localhost:3000",
    reuseExistingServer: true,
  },
});
