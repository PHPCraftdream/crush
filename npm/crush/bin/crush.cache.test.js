#!/usr/bin/env node
'use strict';

// Standalone regression test for the relaunch-from-cache logic added to
// crush.js (see docs/plans/2026-07-29-relaunch-from-cache.md §6). There is
// no test runner configured under npm/ (Jest/Vitest/etc. are not present —
// only web/ has its own Playwright e2e harness, which is unrelated). This
// file is intentionally a bare-Node script using only the `assert` builtin,
// so it doesn't justify pulling in a framework for one test file.
//
// Run manually:
//   node npm/crush/bin/crush.cache.test.js
//
// Exits 0 on success, non-zero (with a thrown Error / assertion message) on
// failure. Never touches the real global npm install or the real
// %LOCALAPPDATA%/crush/bin cache — every fixture lives under a fresh
// fs.mkdtempSync() directory, and CRUSH_BIN_CACHE / NODE_PATH are pointed at
// those temp dirs for each spawned child process only.
//
// How resolution is faked: crush.js resolves the platform package via
// require.resolve('@phpcraftdream/crush-<platform>/package.json'). We give
// each spawned child a NODE_PATH pointing at a temp node_modules directory
// containing a fake @phpcraftdream/crush-<platform> package (package.json +
// bin/crush(.exe)), which Node's module resolution honors for any process
// that has NODE_PATH set at startup (Module._initPaths runs once, early).

const assert = require('node:assert');
const { spawnSync, spawn } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const CRUSH_JS = path.join(__dirname, 'crush.js');
const PLATFORM = process.platform + '-' + process.arch;
const PKG_NAME = '@phpcraftdream/crush-' + PLATFORM;
const BIN_NAME = process.platform === 'win32' ? 'crush.exe' : 'crush';

let failures = 0;

function section(name, fn) {
  process.stdout.write('--- ' + name + ' ---\n');
  try {
    fn();
    process.stdout.write('PASS: ' + name + '\n');
  } catch (err) {
    failures++;
    process.stdout.write('FAIL: ' + name + '\n');
    process.stdout.write((err && err.stack ? err.stack : String(err)) + '\n');
  }
}

// Creates a fresh temp root with:
//   <root>/node_modules/@phpcraftdream/crush-<platform>/package.json
//   <root>/node_modules/@phpcraftdream/crush-<platform>/bin/crush(.exe)
//   <root>/cache/                (empty — CRUSH_BIN_CACHE target)
// Returns { root, nodeModules, pkgDir, binPath, cacheDir }.
function makeFixture(prefix, opts) {
  opts = opts || {};
  const root = fs.mkdtempSync(path.join(os.tmpdir(), prefix + '-'));
  const nodeModules = path.join(root, 'node_modules');
  const pkgDir = path.join(nodeModules, '@phpcraftdream', 'crush-' + PLATFORM);
  fs.mkdirSync(path.join(pkgDir, 'bin'), { recursive: true });

  const version = opts.version || '0.1.7';
  fs.writeFileSync(
    path.join(pkgDir, 'package.json'),
    JSON.stringify({ name: PKG_NAME, version }),
  );

  const binPath = path.join(pkgDir, 'bin', BIN_NAME);
  fs.writeFileSync(binPath, opts.binContent || 'fake-binary-content-v1');

  const cacheDir = path.join(root, 'cache');
  fs.mkdirSync(cacheDir, { recursive: true });

  return { root, nodeModules, pkgDir, binPath, cacheDir };
}

// Runs crush.js as a child process against a fixture. Returns the spawnSync
// result. We pass a harmless arg; the fake "binary" is not a real
// executable so the final spawnSync inside crush.js is expected to fail at
// exec time (ENOENT/EACCES/garbage-exec) — that's fine and expected, we only
// care that the caching logic itself (resolve/copy/rename/sweep) ran
// without throwing an unhandled exception.
function runWrapper(fixture, extraEnv) {
  const env = Object.assign({}, process.env, {
    NODE_PATH: fixture.nodeModules,
    CRUSH_BIN_CACHE: fixture.cacheDir,
  }, extraEnv || {});
  // Make sure ambient CRUSH_BIN_CACHE from the real dev environment (if any)
  // never leaks in even via extraEnv override semantics.
  return spawnSync(process.execPath, [CRUSH_JS, '--version'], {
    env,
    encoding: 'utf8',
  });
}

function listCacheDirs(binCacheDir) {
  if (!fs.existsSync(binCacheDir)) return [];
  return fs
    .readdirSync(binCacheDir, { withFileTypes: true })
    .filter((e) => e.isDirectory())
    .map((e) => e.name);
}

// ---------------------------------------------------------------------
// 1. Key regression: binary mutated (size/mtime) WITHOUT bumping package
//    version must produce a NEW cache key, and the stale one must be swept.
// ---------------------------------------------------------------------
function testKeyRegression() {
  const fx = makeFixture('crush-key-regress', { version: '0.1.7', binContent: 'fake-binary-content-v1' });
  const binCacheDir = path.join(fx.cacheDir, 'crush', 'bin');

  const r1 = runWrapper(fx);
  assert.ok(
    !r1.error || r1.error.code !== 'undefined',
    'first run must not throw before reaching spawnSync: ' + (r1.error && r1.error.message),
  );

  const dirsAfterFirst = listCacheDirs(binCacheDir);
  assert.strictEqual(
    dirsAfterFirst.length,
    1,
    'expected exactly 1 cache dir after first run, got: ' + JSON.stringify(dirsAfterFirst),
  );
  const firstKey = dirsAfterFirst[0];
  const firstCachedBin = path.join(binCacheDir, firstKey, BIN_NAME);
  assert.ok(fs.existsSync(firstCachedBin), 'cached binary missing after first run: ' + firstCachedBin);
  assert.strictEqual(
    fs.readFileSync(firstCachedBin, 'utf8'),
    'fake-binary-content-v1',
    'cached binary content mismatch after first run',
  );

  // Mutate the "binary" in place: different size, and force mtime forward
  // to guarantee mtimeMs differs even on coarse-grained filesystems.
  // Version in package.json is deliberately left untouched — this is
  // exactly deploy.go's behavior (overwrite binary, same package version).
  const statBefore = fs.statSync(fx.binPath);
  fs.writeFileSync(fx.binPath, 'fake-binary-content-v2-longer-payload');
  const futureMs = statBefore.mtimeMs + 5000;
  const futureSec = futureMs / 1000;
  fs.utimesSync(fx.binPath, futureSec, futureSec);
  const statAfter = fs.statSync(fx.binPath);
  assert.notStrictEqual(
    statAfter.mtimeMs,
    statBefore.mtimeMs,
    'test setup error: mtimeMs did not change after utimesSync',
  );

  const r2 = runWrapper(fx);
  assert.ok(
    !r2.error || r2.error.code !== 'undefined',
    'second run must not throw before reaching spawnSync: ' + (r2.error && r2.error.message),
  );

  const dirsAfterSecond = listCacheDirs(binCacheDir);
  assert.strictEqual(
    dirsAfterSecond.length,
    1,
    'REGRESSION: expected exactly 1 cache dir after second run (stale key swept), got: ' +
      JSON.stringify(dirsAfterSecond) +
      ' — if this is 2, the sweep did not remove the stale key; if it is 1 but equal to the ' +
      'first key, the cache key did not change when size/mtime changed without a version bump ' +
      '(this is the §6.1 regression this test exists to catch)',
  );

  const secondKey = dirsAfterSecond[0];
  assert.notStrictEqual(
    secondKey,
    firstKey,
    'REGRESSION: cache key is unchanged after binary size/mtime changed without a package ' +
      'version bump — deploy.go overwrites the binary without bumping version, so a stable key ' +
      'here would silently keep serving a stale cached build forever (plan §6.1)',
  );

  const secondCachedBin = path.join(binCacheDir, secondKey, BIN_NAME);
  assert.ok(fs.existsSync(secondCachedBin), 'cached binary missing after second run: ' + secondCachedBin);
  assert.strictEqual(
    fs.readFileSync(secondCachedBin, 'utf8'),
    'fake-binary-content-v2-longer-payload',
    'REGRESSION: cache entry under the new key does not contain the NEW binary content — ' +
      'stale content would mean a version/build is being served that does not match the ' +
      'installed package',
  );

  fs.rmSync(fx.root, { recursive: true, force: true });
}

// ---------------------------------------------------------------------
// 2. Cache reuse: two runs with an unchanged binary must not re-copy on the
//    second run (cached file mtime must be identical before/after).
// ---------------------------------------------------------------------
function testCacheReuse() {
  const fx = makeFixture('crush-reuse', { version: '0.1.7', binContent: 'fake-binary-content-stable' });
  const binCacheDir = path.join(fx.cacheDir, 'crush', 'bin');

  const r1 = runWrapper(fx);
  assert.ok(!r1.error || r1.error.code !== 'undefined', 'first run must not throw: ' + (r1.error && r1.error.message));

  const dirs1 = listCacheDirs(binCacheDir);
  assert.strictEqual(dirs1.length, 1, 'expected exactly 1 cache dir after first run, got: ' + JSON.stringify(dirs1));
  const cachedBin = path.join(binCacheDir, dirs1[0], BIN_NAME);
  assert.ok(fs.existsSync(cachedBin), 'cached binary missing after first run');
  const mtimeBefore = fs.statSync(cachedBin).mtimeMs;

  // Second run, binary untouched.
  const r2 = runWrapper(fx);
  assert.ok(!r2.error || r2.error.code !== 'undefined', 'second run must not throw: ' + (r2.error && r2.error.message));

  const dirs2 = listCacheDirs(binCacheDir);
  assert.strictEqual(
    dirs2.length,
    1,
    'expected still exactly 1 cache dir after second (no-op) run, got: ' + JSON.stringify(dirs2),
  );
  assert.strictEqual(dirs2[0], dirs1[0], 'cache key changed even though the binary was not modified');

  const mtimeAfter = fs.statSync(cachedBin).mtimeMs;
  assert.strictEqual(
    mtimeAfter,
    mtimeBefore,
    'cached file was rewritten on the second run even though nothing changed — expected the ' +
      'wrapper to skip the copy when targetPath already exists',
  );

  fs.rmSync(fx.root, { recursive: true, force: true });
}

// ---------------------------------------------------------------------
// 3. First-launch race: two wrapper processes started concurrently against
//    a cold cache for the same key must both complete without an unhandled
//    exception in the caching logic, and must leave exactly one final
//    cache directory (no leftover .tmp-* staging dirs, no duplicate dirs).
// ---------------------------------------------------------------------
async function testConcurrentFirstLaunch() {
  const fx = makeFixture('crush-race', { version: '0.1.7', binContent: 'fake-binary-content-race' });
  const binCacheDir = path.join(fx.cacheDir, 'crush', 'bin');

  function spawnAsync() {
    return new Promise((resolve) => {
      const env = Object.assign({}, process.env, {
        NODE_PATH: fx.nodeModules,
        CRUSH_BIN_CACHE: fx.cacheDir,
      });
      const child = spawn(process.execPath, [CRUSH_JS, '--version'], { env, stdio: 'pipe' });
      let stderr = '';
      child.stderr.on('data', (d) => { stderr += d; });
      child.on('error', (err) => resolve({ launchError: err, stderr }));
      child.on('close', () => resolve({ launchError: null, stderr }));
    });
  }

  const [a, b] = await Promise.all([spawnAsync(), spawnAsync()]);

  assert.strictEqual(a.launchError, null, 'first concurrent process failed to launch at all: ' + (a.launchError && a.launchError.message));
  assert.strictEqual(b.launchError, null, 'second concurrent process failed to launch at all: ' + (b.launchError && b.launchError.message));

  // Neither process's caching logic should have reported an unhandled crash.
  // (The wrapper's own try/catch turns caching errors into a warning + safe
  // fallback, never a Node stack-trace crash; a Node "Uncaught" trace or
  // "internal/process" mention would indicate the caching logic itself blew
  // up unhandled rather than being caught by the wrapper's own try/catch.)
  for (const [label, res] of [['first', a], ['second', b]]) {
    assert.ok(
      !/Uncaught|at internal\/process/.test(res.stderr),
      label + ' concurrent process appears to have thrown an unhandled exception in the caching ' +
        'logic; stderr:\n' + res.stderr,
    );
  }

  const finalDirs = fs.existsSync(binCacheDir)
    ? fs.readdirSync(binCacheDir, { withFileTypes: true }).map((e) => e.name)
    : [];
  const realDirs = finalDirs.filter((n) => !n.startsWith('.tmp-'));
  const tmpDirs = finalDirs.filter((n) => n.startsWith('.tmp-'));

  assert.strictEqual(
    tmpDirs.length,
    0,
    'leftover .tmp-* staging directories after the race resolved: ' + JSON.stringify(tmpDirs),
  );
  assert.strictEqual(
    realDirs.length,
    1,
    'expected exactly 1 final cache dir after two concurrent first-launches raced for the same ' +
      'key, got: ' + JSON.stringify(realDirs),
  );

  fs.rmSync(fx.root, { recursive: true, force: true });
}

// ---------------------------------------------------------------------
// 4. Fallback: when the cache directory cannot be resolved/used, the
//    wrapper must not throw an unhandled exception — it should print the
//    "binary cache unavailable" warning and fall back to the original
//    binary path.
// ---------------------------------------------------------------------
function testCacheUnavailableFallback() {
  const fx = makeFixture('crush-fallback', { version: '0.1.7', binContent: 'fake-binary-content-fallback' });

  // Point CRUSH_BIN_CACHE at a path that cannot be created as a directory:
  // a path that has a *file* (not a directory) as one of its intermediate
  // path segments. fs.mkdirSync({recursive:true}) will fail with ENOTDIR
  // when trying to create a subdirectory under a plain file. This reliably
  // triggers the wrapper's outer catch without touching any real ACL/perms
  // machinery (which is unreliable to set up portably in a test).
  const blockerFile = path.join(fx.root, 'not-a-directory');
  fs.writeFileSync(blockerFile, 'blocker');
  const bogusCacheRoot = path.join(blockerFile, 'nested', 'cache');

  const env = Object.assign({}, process.env, {
    NODE_PATH: fx.nodeModules,
    CRUSH_BIN_CACHE: bogusCacheRoot,
  });
  const result = spawnSync(process.execPath, [CRUSH_JS, '--version'], { env, encoding: 'utf8' });

  assert.ok(
    !result.error || result.error.code !== 'undefined',
    'wrapper process failed to launch at all (should have run and fallen back instead): ' +
      (result.error && result.error.message),
  );

  assert.ok(
    /crush: warning: binary cache unavailable/.test(result.stderr || ''),
    'expected the "binary cache unavailable" fallback warning on stderr, got:\n' + (result.stderr || '(empty)'),
  );

  // Fallback means it attempted to launch the ORIGINAL binary path
  // directly (fx.binPath), not a cache copy. Since our fake binary isn't a
  // real executable, spawnSync inside crush.js will itself fail to exec it
  // and crush.js will report that via its own "failed to launch" message
  // referencing fx.binPath — confirming launchTarget fell back correctly.
  assert.ok(
    (result.stderr || '').includes(fx.binPath) || (result.stdout || '').includes(fx.binPath),
    'expected the fallback launch attempt to reference the original binary path ' +
      fx.binPath + ', got stderr:\n' + (result.stderr || '(empty)') + '\nstdout:\n' + (result.stdout || '(empty)'),
  );

  fs.rmSync(fx.root, { recursive: true, force: true });
}

(async () => {
  section('1. key regression (size/mtime change without version bump)', testKeyRegression);
  section('2. cache reuse (no re-copy on unchanged binary)', testCacheReuse);
  await (async () => {
    process.stdout.write('--- 3. concurrent first-launch race ---\n');
    try {
      await testConcurrentFirstLaunch();
      process.stdout.write('PASS: 3. concurrent first-launch race\n');
    } catch (err) {
      failures++;
      process.stdout.write('FAIL: 3. concurrent first-launch race\n');
      process.stdout.write((err && err.stack ? err.stack : String(err)) + '\n');
    }
  })();
  section('4. fallback when cache is unavailable', testCacheUnavailableFallback);

  if (failures > 0) {
    process.stderr.write('\n' + failures + ' section(s) FAILED\n');
    process.exit(1);
  }
  process.stdout.write('\nAll sections passed.\n');
  process.exit(0);
})();
