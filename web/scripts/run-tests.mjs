#!/usr/bin/env node
// Discovers a single free TCP port and invokes `playwright test ...` with that
// port exposed as E2E_PORT in the child environment.
//
// Why this exists: playwright.config.ts is evaluated as a module by BOTH the
// parent process (which spawns the webServer) and every worker process (which
// reads use.baseURL). If the port is discovered inside the config module, each
// independent module evaluation calls findFreePort() again and gets a DIFFERENT
// random port, so the webServer listens on port A while a worker navigates to
// port B → net::ERR_CONNECTION_REFUSED.
//
// Fix: discover the port exactly ONCE here, in the orchestrating process, then
// hand it to playwright via an env var (E2E_PORT) that the config reads
// SYNCHRONOUSLY (no re-discovery) on every module load. All processes inherit
// the same E2E_PORT, so they all agree.
//
// Invoked by `npm run test` / `npm run test:ui`. All argv after the script name
// are forwarded to `playwright test`.

import net from "node:net";
import { spawn } from "node:child_process";

function findFreePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on("error", reject);
    // Port 0 = "assign me any free ephemeral port".
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

function play() {
  // The config honors an explicit E2E_PORT to allow `E2E_PORT=4321 npm run test`
  // (e.g. pointing at an already-running dev server). Only discover when unset.
  if (process.env.E2E_PORT && /^\d+$/.test(process.env.E2E_PORT)) {
    return Promise.resolve(Number(process.env.E2E_PORT));
  }
  return findFreePort();
}

const port = await play();

// Forward all argv after the script name to `playwright test`. Resolve the
// `playwright` binary via PATH (npm puts node_modules/.bin on PATH for script
// execution). On Windows node_modules/.bin/playwright is a .cmd shim that node
// cannot exec directly, so shell:true is required there only. On POSIX we pass
// shell:false to avoid DEP0190 (args-not-escaped) and to forward argv verbatim.
const args = ["test", ...process.argv.slice(2)];
const env = { ...process.env, E2E_PORT: String(port) };

const child = spawn("playwright", args, {
  stdio: "inherit",
  env,
  shell: process.platform === "win32",
});

child.on("error", (err) => {
  console.error("Failed to spawn playwright:", err);
  process.exitCode = 1;
});

child.on("exit", (code, signal) => {
  if (signal) {
    // Forward signals by re-killing the parent; npm handles exit code.
    process.kill(process.pid, signal);
  } else {
    process.exitCode = code ?? 1;
  }
});
