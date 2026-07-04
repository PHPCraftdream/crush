#!/usr/bin/env node
'use strict';

// Minimal platform-binary launcher for the @CRUSH_FORK_SCOPE/crush npm
// package. Zero dependencies — Node builtins only. It resolves the
// prebuilt binary shipped by the matching optional platform package, then
// re-execs it with argv passthrough (spawnSync, argv array, never shell).
//
// Maintainer note: @CRUSH_FORK_SCOPE is a literal placeholder. Find-and-
// replace it with the real npm scope before publishing.

const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const SCOPE = '@CRUSH_FORK_SCOPE';
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

var result = spawnSync(binary, process.argv.slice(2), { stdio: 'inherit' });

// Spawn-time failure (ENOENT/EACCES) — the binary couldn't be launched.
if (result.error) {
  process.stderr.write('crush: failed to launch ' + binary + ': ' + result.error.message + '\n');
  process.exit(1);
}

// Forward a fatal signal to the launcher so the exit behaviour matches a
// native exec as closely as possible.
if (result.signal) {
  process.kill(process.pid, result.signal);
  process.exit(1); // Fallback if the signal is not delivered synchronously.
}

process.exit(result.status == null ? 1 : result.status);
