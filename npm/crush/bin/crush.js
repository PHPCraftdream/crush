#!/usr/bin/env node
'use strict';

// Minimal platform-binary launcher for the @phpcraftdream/crush npm
// package. Zero dependencies — Node builtins only. It resolves the
// prebuilt binary shipped by the matching optional platform package, then
// re-execs it with argv passthrough (spawnSync, argv array, never shell).

const { spawnSync } = require('node:child_process');
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
// then always exec the cached copy. The cache key folds in size+mtimeMs of
// the *original* file (not just the npm package version) because deploy.go
// can silently overwrite the platform package's binary without bumping the
// package version — a bare-version key would keep serving a stale cached
// build forever (see plan §6.1).
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
  const stat = fs.statSync(binary);
  let pkgVersion = '';
  try {
    pkgVersion = JSON.parse(fs.readFileSync(path.join(pkgDir, 'package.json'), 'utf8')).version || '';
  } catch (_) {
    // package.json missing/unreadable — key still varies by size+mtime.
  }

  const keyInput = stat.size + ':' + stat.mtimeMs + ':' + pkgVersion;
  const key = fnv1a64Hex(keyInput);

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

// Tiny 64-bit FNV-1a implementation using BigInt so it doesn't lose
// precision the way plain `number` arithmetic would past 2^53. Returns a
// fixed-width hex string suitable for use as a cache directory name.
function fnv1a64Hex(str) {
  const OFFSET_BASIS = 0xcbf29ce484222325n;
  const PRIME = 0x100000001b3n;
  const MASK = 0xffffffffffffffffn; // wrap to 64 bits

  let hash = OFFSET_BASIS;
  for (let i = 0; i < str.length; i++) {
    hash ^= BigInt(str.charCodeAt(i));
    hash = (hash * PRIME) & MASK;
  }
  return hash.toString(16).padStart(16, '0');
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
