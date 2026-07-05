# Changelog

All notable changes to the `@phpcraftdream/crush` npm distribution are
documented here. Versions correspond to the `npm-vX.Y.Z` git tags that
trigger [`publish-fork-npm.yml`](../../.github/workflows/publish-fork-npm.yml).

This is the npm-package changelog, not the fork's engineering decision
log — see [`CHANGELOG.fork.md`](../../CHANGELOG.fork.md) at the repo
root for the full per-file merge/divergence history.

## [0.1.1]

- `crush models list` no longer hits the network or writes the provider
  cache by default; pass `--refresh` to force a fresh fetch.
- `crush ping --role smart|fast` — ping either model slot without
  switching to `ping-fast`.
- `ZHIPU_API_KEY` accepted as a fallback for the Z.AI provider when
  `ZAI_API_KEY` is unset.
- Startup diagnostics ("no git repository detected", "Apple Terminal
  detected") no longer print on scripted/default runs — only under
  `--debug` (or `"options": {"debug": true}` in config).
- Hardened version reporting: an ldflags-injected release version can no
  longer be silently overwritten by Go's module metadata, and the npm
  publish workflow verifies the packaged binary reports the expected
  version before publishing.

## [0.1.0]

- First release of the npm distribution: `npm install -g
  @phpcraftdream/crush` installs a prebuilt, Go-free binary via
  per-platform optional dependencies (linux/darwin × x64/arm64,
  win32 x64).
