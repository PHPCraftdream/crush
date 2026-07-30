#!/usr/bin/env node
'use strict';

// Minimal platform-binary launcher for the @phpcraftdream/crush npm
// package. Zero dependencies — Node builtins only. It resolves the
// prebuilt binary shipped by the matching optional platform package, then
// re-execs it with argv passthrough (spawnSync, argv array, never shell).

const { spawnSync } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const SCOPE = '@phpcraftdream';
const platform = process.platform + '-' + process.arch;
const pkgName = SCOPE + '/crush-' + platform;
const binName = process.platform === 'win32' ? 'crush.exe' : 'crush';

// Resolve the installed platform package directory via its package.json —
// the one file guaranteed to exist with a resolvable extension.
let pkgDir;
try {
  pkgDir = path.dirname(require.resolve(pkgName + '/package.json'));
} catch (_) {
  process.stderr.write(
    'crush: the platform package "' + pkgName + '" is not installed.\n' +
    'This is an unofficial npm build of crush (a fork of charmbracelet/crush).\n' +
    'Supported platforms: linux-x64, linux-arm64, darwin-x64, darwin-arm64, win32-x64.\n' +
    'Reinstall ' + SCOPE + '/crush, or install ' + pkgName + ' manually.\n',
  );
  process.exit(127);
}

const binary = path.join(pkgDir, 'bin', binName);
if (!fs.existsSync(binary)) {
  process.stderr.write(
    'crush: platform package "' + pkgName + '" is installed but its binary is missing:\n' +
    '  ' + binary + '\n' +
    'The package may be corrupted; try reinstalling.\n',
  );
  process.exit(127);
}

// --- Relaunch-from-cache -----------------------------------------------
//
// Never spawn `binary` directly out of node_modules: that keeps the file
// locked for the lifetime of the session, and npm can't overwrite a locked
// file on the next `npm install` (the exact failure mode that motivated
// this wrapper — see docs/plans/2026-07-29-relaunch-from-cache.md §6).
//
// Instead: copy the resolved binary into a per-build cache directory once,
// then always exec the cached copy. The cache key is the SHA-256 content
// hash of the *original* file, not size+mtimeMs+version: a stat-based key
// is spoofable — any tool that overwrites the binary in place while
// preserving its size and mtime (e.g. a deploy/copy step that explicitly
// sets mtime) would collide with the old key and keep serving the stale
// cached build forever, even though the content changed (see plan §6.1
// and P3.4 in docs/reviews). Hashing the actual bytes closes that hole:
// the cache key can only match if the content is identical.
//
// The key lives in the cache *directory* name, never in the binary's own
// file name: agentguard's Tier-3 self-block (agentguard.go) matches the
// recursive-crush denylist against the process image's base name, so the
// cached copy must still be literally "crush.exe" / "crush".
//
// Any failure anywhere in this block falls back to the pre-existing
// behaviour: spawn `binary` straight out of node_modules.
let launchTarget = binary;
try {
  const key = hashFileSync(binary);

  const cacheRoot = resolveCacheRoot();
  const binCacheDir = path.join(cacheRoot, 'crush', 'bin');
  const targetDir = path.join(binCacheDir, key);
  const targetPath = path.join(targetDir, binName);

  if (!fs.existsSync(targetPath)) {
    fs.mkdirSync(binCacheDir, { recursive: true });

    // Unique-per-process tmp dir so two concurrent first-launches never
    // write into the same staging path (plan §10 Г5).
    const tmpDir = path.join(
      binCacheDir,
      '.tmp-' + process.pid + '-' + process.hrtime.bigint().toString(36),
    );
    fs.mkdirSync(tmpDir, { recursive: true });
    const tmpPath = path.join(tmpDir, binName);
    fs.copyFileSync(binary, tmpPath);
    if (process.platform !== 'win32') {
      fs.chmodSync(tmpPath, 0o755);
    }

    try {
      fs.renameSync(tmpDir, targetDir);
    } catch (renameErr) {
      // Another process won the race and already created (and may already
      // be executing) targetDir. That's a normal outcome, not an error —
      // just drop our staging copy and use the winner's.
      if (fs.existsSync(targetPath)) {
        try {
          fs.rmSync(tmpDir, { recursive: true, force: true });
        } catch (_) {
          // best-effort cleanup only
        }
      } else {
        throw renameErr;
      }
    }
  }

  // Best-effort sweep of stale build caches. Never allowed to block or
  // fail the actual launch — every removal is individually guarded, and
  // the whole block is wrapped again below by the outer try/catch.
  try {
    for (const entry of fs.readdirSync(binCacheDir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      if (entry.name === key) continue;
      if (entry.name.startsWith('.tmp-')) continue; // mid-copy by another process
      try {
        fs.rmSync(path.join(binCacheDir, entry.name), { recursive: true, force: true });
      } catch (_) {
        // Directory (or a file inside it) is held open by a live process
        // on this or another build's key — leave it for a later sweep.
      }
    }
  } catch (_) {
    // Sweeping is pure housekeeping; never let it affect the launch.
  }

  launchTarget = targetPath;
} catch (cacheErr) {
  process.stderr.write(
    'crush: warning: binary cache unavailable (' + cacheErr.message + '); ' +
    'running directly from the installed package.\n',
  );
  launchTarget = binary;
}

var result = spawnSync(launchTarget, process.argv.slice(2), { stdio: 'inherit' });

// Spawn-time failure (ENOENT/EACCES) — the binary couldn't be launched.
if (result.error) {
  process.stderr.write('crush: failed to launch ' + launchTarget + ': ' + result.error.message + '\n');
  process.exit(1);
}

// Forward a fatal signal to the launcher so the exit behaviour matches a
// native exec as closely as possible.
if (result.signal) {
  process.kill(process.pid, result.signal);
  process.exit(1); // Fallback if the signal is not delivered synchronously.
}

process.exit(result.status == null ? 1 : result.status);

// --- helpers -------------------------------------------------------------

// hashFileSync returns the lowercase hex SHA-256 of the file at
// absolutePath, computed via a stream so memory use stays flat regardless
// of binary size (crush binaries run tens of MB) — the whole file is never
// read into memory at once.
//
// Cost note: hashing runs on every invocation (see the cache-key doc
// comment above for why a stat-based size+mtime key isn't sufficient), and
// costs roughly the time to read the file once from disk/page-cache
// (order of ~100ms-1s for a ~60MB binary depending on cache warmth) — cheap
// relative to spawning a child process and negligible next to the
// non-interactive-agent-tooling workloads this CLI targets, so no
// cache-the-hash-itself shortcut is used: any such shortcut would have to
// be keyed by the same spoofable size+mtime this fix exists to stop
// trusting.
function hashFileSync(absolutePath) {
  const hash = crypto.createHash('sha256');
  const fd = fs.openSync(absolutePath, 'r');
  try {
    const buf = Buffer.alloc(1 << 20); // 1 MiB read buffer
    let bytesRead;
    let position = 0;
    while ((bytesRead = fs.readSync(fd, buf, 0, buf.length, position)) > 0) {
      hash.update(bytesRead === buf.length ? buf : buf.subarray(0, bytesRead));
      position += bytesRead;
    }
  } finally {
    fs.closeSync(fd);
  }
  return hash.digest('hex');
}

// Node has no os.UserCacheDir() equivalent — compute the per-platform
// cache root by hand, with an explicit operator override.
function resolveCacheRoot() {
  if (process.env.CRUSH_BIN_CACHE) {
    return process.env.CRUSH_BIN_CACHE;
  }
  if (process.platform === 'win32') {
    return process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local');
  }
  if (process.platform === 'darwin') {
    return path.join(os.homedir(), 'Library', 'Caches');
  }
  return process.env.XDG_CACHE_HOME || path.join(os.homedir(), '.cache');
}
