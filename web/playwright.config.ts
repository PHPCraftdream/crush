import { defineConfig } from "@playwright/test";

/**
 * Resolve the dev-server port for this test run.
 *
 * The port MUST be identical in every process that evaluates this config
 * module: the parent process (which spawns `webServer`) and every worker
 * (which reads `use.baseURL`). Discovering a free port inside the config
 * module is BROKEN — each independent module evaluation would call
 * findFreePort() again and get a DIFFERENT random port, so the webServer
 * would listen on port A while a worker navigated to port B
 * (net::ERR_CONNECTION_REFUSED).
 *
 * Fix: a wrapper script (scripts/run-tests.mjs, wired into `npm run test`
 * and `npm run test:ui`) discovers ONE free port before invoking Playwright
 * and exports it as E2E_PORT. We read that env var SYNCHRONOUSLY here, with
 * no re-discovery, so every process inherits the same agreed-upon value.
 *
 * Running `playwright test` directly (without the wrapper) has no guaranteed
 * cross-process port agreement; in that case we fall back to a static default
 * port with reuseExistingServer. The wrapper is the supported path.
 */
function resolvePort(): number {
  const envPort = process.env.E2E_PORT;
  if (envPort && /^\d+$/.test(envPort)) {
    return Number(envPort);
  }
  // Fallback for direct `playwright test` invocation (no wrapper). Keeps the
  // historical ad-hoc-run behavior (static 3000 + reuseExistingServer) rather
  // than re-introducing the broken per-module-evaluation discovery.
  return 3000;
}

const port = resolvePort();
const baseURL = `http://localhost:${port}`;

export default defineConfig({
  testDir: "./tests",
  workers: "50%",
  use: {
    baseURL,
    // Recognize both data-testid and data-test-id attributes
    testIdAttribute: "data-test-id",
  },
  webServer: {
    command: "npm run dev",
    url: baseURL,
    // `env` REPLACES process.env for the spawned command (per Playwright's
    // TestConfigWebServer type), so we must spread the current env and only
    // overlay PORT. rsbuild reads PORT in rsbuild.config.ts.
    env: { ...process.env, PORT: String(port) } as { [key: string]: string },
    // When E2E_PORT was set by the wrapper, the port was verified free moments
    // before rsbuild bound it, so a stray unrelated server cannot be spuriously
    // matched. The 3000 fallback path keeps reuseExistingServer for parity with
    // the historical ad-hoc-run behavior.
    reuseExistingServer: true,
  },
});
