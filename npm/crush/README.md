# `@phpcraftdream/crush`

**Unofficial npm distribution of [crush](https://github.com/charmbracelet/crush),
maintained as a fork. Not published by Charmbracelet.**

This package installs the `crush` CLI as a single prebuilt binary — **no Go
toolchain, no pnpm, no build step** on the user's machine. The correct binary
for your OS/arch is pulled in automatically as an npm optional dependency
(the same distribution model `esbuild` uses).

## Install

```sh
npm install -g @phpcraftdream/crush
```

Then run:

```sh
crush --version
crush
```

## Supported platforms

| npm package                         | OS      | Arch |
| ----------------------------------- | ------- | ---- |
| `@phpcraftdream/crush-linux-x64`   | Linux   | x64  |
| `@phpcraftdream/crush-linux-arm64` | Linux   | arm64 |
| `@phpcraftdream/crush-darwin-x64`  | macOS   | x64  |
| `@phpcraftdream/crush-darwin-arm64`| macOS   | arm64 (Apple Silicon) |
| `@phpcraftdream/crush-win32-x64`   | Windows | x64  |

The launcher (`bin/crush.js`) resolves the matching package and execs its
binary with argv passthrough. If your platform has no package, it exits with
a clear message.

## Scope

Packages publish under the `@phpcraftdream` npm scope.

## Licensing & redistribution

crush is licensed under the **Functional Source License, FSL-1.1-MIT**,
© 2025–2026 Charmbracelet, Inc. The full text is shipped in `LICENSE` and at
the repository root as `LICENSE.md`. FSL permits non-competing use and
redistribution provided the license and copyright notice are preserved; it
converts to MIT after two years. This npm repackaging is an unofficial
convenience distribution and does not change the license of the underlying
software.

## Reporting issues

For bugs specific to this npm packaging (missing binaries, wrong platform
selection, launcher errors), file an issue against this fork. For bugs in
crush itself, report them upstream at
<https://github.com/charmbracelet/crush/issues>.
