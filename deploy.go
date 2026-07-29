//go:build ignore

// deploy.go — one-shot: prod-build the binary, then replace whatever
// `crush` resolves to on PATH with the freshly built artifact, WITHOUT
// killing any running crush process. Verifies by running the new
// binary's --version.
//
// Live sessions are safe by construction, not by avoidance:
//   Unix:    replaceFile does a temp-write + rename over dst. A running
//            process keeps its open file descriptor pointing at the old
//            inode's data even after the directory entry is replaced, so
//            in-flight `crush` processes are completely unaffected.
//   Windows: renaming a file out from under a running .exe is allowed
//            (the OS only cares about the open data, not the name), but
//            DELETING that renamed-aside file while the process still
//            holds it open is not — it fails with "Access is denied."
//            So replaceFile renames dst aside to "dst.old-<token>"
//            instead of deleting it, then writes the new binary under
//            the now-free dst. The old file lingers under its new name
//            until every process still running it exits, at which point
//            a LATER deploy's cleanup sweep (SweepRenameAsideLeftovers)
//            can finally remove it — failures during that sweep are
//            silently ignored, since "still busy" just means a live
//            session is still serving that build.
//            (Confirmed by hand on 2026-07-29, see
//            docs/plans/2026-07-29-relaunch-from-cache.md §2.)
//
// If NO crush exists on PATH yet, this is a first install: the binary
// goes to the standard per-user location for the OS and the directory
// is made reachable from the command line —
//   Windows:      %LOCALAPPDATA%\Programs\crush\crush.exe
//                 (dir appended to the user PATH via the registry;
//                 new terminals pick it up, current ones need restart)
//   Linux/macOS:  ~/.local/bin/crush
//                 (already on PATH in most distros; if not, a ready
//                 export line is printed — shell rc files are never
//                 edited automatically)
//
// Usage:   go run deploy.go
// Override the destination path with CRUSH_DEPLOY_PATH=...
//
// The decision logic behind path/PATH handling (install location,
// PATH-membership checks, executable-vs-shim detection, cwd-excluding
// PATH lookup) as well as the rename-aside naming and leftover-sweep
// logic lives in internal/deploy so it is unit-tested by `go test ./...`
// on every OS in CI, even though this file itself is go:build ignore and
// only ever runs via `go run`.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/deploy"
)

// ts returns the current local time as an "HH:MM:SS" prefix so every line
// this script prints carries a timestamp — useful for spotting exactly
// where a deploy hung (e.g. a stuck build or a slow PATH-registry call).
func ts() string { return time.Now().Format("15:04:05") }

func step(msg string, args ...any) { fmt.Printf("[%s] → "+msg+"\n", append([]any{ts()}, args...)...) }
func ok(msg string, args ...any)   { fmt.Printf("[%s] ✓ "+msg+"\n", append([]any{ts()}, args...)...) }
func warn(msg string, args ...any) { fmt.Printf("[%s] ! "+msg+"\n", append([]any{ts()}, args...)...) }
func fatal(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] ✗ "+msg+"\n", append([]any{ts()}, args...)...)
	os.Exit(1)
}

func mustRun(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("%s %v: %v", name, args, err)
	}
}

// runQuiet runs a command and returns (stdout, ok). A non-zero exit is
// surfaced as ok=false; used for best-effort external calls (currently
// only the PATH-registry reads/writes in ensureOnPath) where the caller
// wants to inspect output without treating a failure as fatal.
func runQuiet(name string, args ...string) (string, bool) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err == nil
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fatal("getwd: %v", err)
	}

	// 1. Production build (delegates to build.go so we don't drift).
	step("Running build.go (web bundle + Go binary with BuildID)…")
	mustRun(root, "go", "run", "build.go")

	src := filepath.Join(root, binaryName())
	if _, err := os.Stat(src); err != nil {
		fatal("expected build artifact at %s: %v", src, err)
	}

	// 2. Find destinations. CRUSH_DEPLOY_PATH wins (single target);
	//    otherwise we collect every crush binary on PATH that should be
	//    kept in sync — typically the npm-shim sibling AND the
	//    node_modules real binary, because Windows cmd's PATH resolution
	//    can pick either depending on the order of npm-dir vs other
	//    entries. Deploying to both stops "crush --version is fresh but
	//    crush claude-init says Unknown flag --replace" puzzles.
	//
	//    If nothing is found, this is a FIRST INSTALL: fall back to the
	//    standard per-user location for the OS and make sure it is
	//    reachable from the command line (see ensureOnPath).
	freshInstall := false
	dsts, err := resolveDests()
	if err != nil {
		dst, derr := deploy.DefaultInstallPath()
		if derr != nil {
			fatal("could not determine an install location: %v", derr)
		}
		warn("no existing crush found to replace: %v", err)
		step("First install → standard per-user location: %s", dst)
		dsts = []string{dst}
		freshInstall = true
	}
	for _, dst := range dsts {
		if deploy.SameFile(src, dst) {
			fatal("source and destination are the same file (%s) — nothing to replace", dst)
		}
	}
	step("Will replace %d path(s):", len(dsts))
	for _, dst := range dsts {
		fmt.Printf("[%s]     %s\n", ts(), dst)
	}

	// 3. Sweep leftover rename-aside files from previous deploys before
	//    doing anything else, so garbage doesn't accumulate run over run.
	//    Failures are expected and silent: a file that won't delete just
	//    means a live session from a previous deploy is still serving it.
	for _, dst := range dsts {
		for _, removed := range deploy.SweepRenameAsideLeftovers(dst) {
			ok("Swept leftover %s", removed)
		}
	}

	// 4. Copy with a temp + rename to make the swap atomic for each
	//    target. Live sessions are never killed: on Unix the rename is
	//    safe for running processes outright; on Windows replaceFile
	//    renames any busy dst aside instead of deleting it. If any single
	//    replace fails we bail — leaving a mixed state is worse than
	//    leaving the original.
	for _, dst := range dsts {
		if err := replaceFile(src, dst); err != nil {
			fatal("replace %s: %v", dst, err)
		}
		ok("Replaced %s", dst)
	}

	// 5. Verify by running the newly installed binary at every
	//    destination — catches mismatched sibling files that would
	//    silently keep an old build alive.
	for _, dst := range dsts {
		if v, err := exec.Command(dst, "--version").CombinedOutput(); err == nil {
			ok("Installed (%s): %s", filepath.Base(dst), strings.TrimSpace(string(v)))
		} else {
			warn("--version probe failed for %s (may be fine for non-exe shim): %v", dst, err)
		}
	}

	// 6. First install only: make sure the install dir is reachable from
	//    the command line on this OS.
	if freshInstall {
		ensureOnPath(filepath.Dir(dsts[0]))
	}
}

// ensureOnPath makes `dir` reachable from the command line.
//
// Windows: appends dir to the USER Path in the registry via
// PowerShell's [Environment]::SetEnvironmentVariable — deliberately NOT
// `setx`, which silently truncates values longer than 1024 chars and
// has destroyed many a PATH. Only new terminals see the change; the
// current one keeps its inherited copy.
//
// Unix: never edits shell rc files (too many shells, too invasive).
// If dir is already in $PATH there is nothing to do; otherwise print a
// ready-to-paste export line and name the usual rc file.
//
// The PATH-membership check and the updated-PATH-string construction
// are pure functions in internal/deploy (unit-tested); only the actual
// registry read/write and env inspection stay here as thin, untested
// side effects.
func ensureOnPath(dir string) {
	if runtime.GOOS == "windows" {
		out, okRun := runQuiet("powershell", "-NoProfile", "-Command",
			"[Environment]::GetEnvironmentVariable('Path','User')")
		if !okRun {
			warn("could not read the user PATH: %s\n  add %s to PATH manually", strings.TrimSpace(out), dir)
			return
		}
		current := strings.TrimSpace(out)
		if deploy.PathListContains(current, dir) {
			ok("%s is already on the user PATH", dir)
			return
		}
		updated := deploy.AppendToPathList(current, dir)
		// Single-quote for PowerShell; ' inside is doubled per PS rules.
		psQuoted := "'" + strings.ReplaceAll(updated, "'", "''") + "'"
		if out, okRun := runQuiet("powershell", "-NoProfile", "-Command",
			"[Environment]::SetEnvironmentVariable('Path', "+psQuoted+", 'User')"); !okRun {
			warn("failed to append %s to the user PATH: %s\n  add it manually (System Properties → Environment Variables)", dir, strings.TrimSpace(out))
			return
		}
		ok("Added %s to the user PATH — restart the terminal to pick it up", dir)
		return
	}

	if deploy.PathListContains(os.Getenv("PATH"), dir) {
		ok("%s is already on PATH", dir)
		return
	}
	warn("%s is not on PATH. Add this line to your shell rc (~/.profile, ~/.bashrc or ~/.zshrc):\n    export PATH=\"$PATH:%s\"", dir, dir)
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "crush.exe"
	}
	return "crush"
}

// npmNodeOS and npmNodeArch map this OS/arch to the Node "os"/"cpu" package
// names used for the fork's per-platform npm packages
// (node_modules/@phpcraftdream/crush-<node_os>-<node_arch>/bin/<binaryName()>),
// mirroring the TARGETS table in .github/workflows/publish-fork-npm.yml.
// Only the pair matching THIS machine needs to resolve correctly — deploy.go
// only ever replaces binaries on the machine it runs on — so we hardcode the
// mapping for the platforms deploy.go actually builds for (see binaryName())
// rather than building a general GOOS/GOARCH → node table.
func npmNodeOS() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS // "linux", "darwin"
}

func npmNodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// resolveDests decides what files to overwrite. Priority:
//  1. $CRUSH_DEPLOY_PATH set → single forced target, used as-is.
//  2. Otherwise we discover the npm-installed crush via exec.LookPath
//     and return EVERY binary we can find around it:
//     a. <npm-dir>/node_modules/@phpcraftdream/crush-<node_os>-<node_arch>/bin/crush(.exe)
//     — the real binary the JS wrapper execs via `node`. The fork ships
//     the binary in the PLATFORM package, not the meta package (unlike
//     upstream's @charmland/crush, which bundled bin/ directly in the
//     package the JS wrapper lives in) — see
//     docs/plans/2026-07-29-relaunch-from-cache.md §5.3.
//     b. <npm-dir>/crush.exe (Windows only) — a sibling native
//     binary that `cmd` may pick BEFORE the JS wrapper depending
//     on PATHEXT and PATH ordering. Historically this slot
//     received an out-of-band copy from a previous install and
//     then drifted, producing "crush --version is fresh but
//     claude-init --replace is unknown" symptoms.
//     c. The LookPath result itself, only if it is an executable
//     (.exe on Windows, no extension on Unix) — covers raw
//     binaries dropped on PATH without an npm package around
//     them.
//
// We deduplicate by absolute path so the same file isn't replaced
// twice on a same-file collision.
func resolveDests() ([]string, error) {
	if env := os.Getenv("CRUSH_DEPLOY_PATH"); env != "" {
		return []string{env}, nil
	}
	// We can't use exec.LookPath here: deploy.go is run from the repo
	// root which itself contains a freshly built crush.exe (the build
	// artifact). Go 1.19+ exec.LookPath refuses to return executables
	// found via the cwd entry of PATH (returns exec.ErrDot) to defend
	// against directory-planting attacks. That defence is exactly
	// wrong for us — we WANT the OTHER crush on PATH (the npm-installed
	// or system one), not the local build artifact. Walk PATH ourselves,
	// skipping anything that resolves to cwd.
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}
	var exts []string
	if runtime.GOOS == "windows" {
		exts = deploy.WindowsPathExts(os.Getenv("PATHEXT"))
	} else {
		exts = []string{""}
	}
	p, err := deploy.LookPathExcludingCwd("crush", cwd, os.Getenv("PATH"), exts)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(p)

	var cands []string
	// (a) node_modules real binary — lives in the fork's per-platform
	// package (@phpcraftdream/crush-<node_os>-<node_arch>), not the meta
	// package (@phpcraftdream/crush) that owns bin/crush.js.
	npmBin := filepath.Join(dir, "node_modules", "@phpcraftdream",
		fmt.Sprintf("crush-%s-%s", npmNodeOS(), npmNodeArch()), "bin", binaryName())
	if _, err := os.Stat(npmBin); err == nil {
		cands = append(cands, npmBin)
	}
	// (b) sibling crush.exe in npm bin dir (Windows only). This is the
	// one that Windows `cmd` may pick when PATHEXT resolves `crush` to
	// `crush.exe` before considering `crush.cmd`/`crush.ps1`.
	if runtime.GOOS == "windows" {
		sibling := filepath.Join(dir, "crush.exe")
		if _, err := os.Stat(sibling); err == nil {
			cands = append(cands, sibling)
		}
	}
	// (c) the LookPath result itself, only if it looks like an
	// executable (not a script/shim). On Windows that means .exe; on
	// Unix it means no extension and an executable mode bit. We add
	// only if not already covered.
	if deploy.IsReplaceableExe(p) {
		cands = append(cands, p)
	}

	// Deduplicate by absolute path resolution — symlinks / case-only
	// differences on Windows shouldn't cause duplicate writes.
	seen := make(map[string]bool, len(cands))
	out := cands[:0]
	for _, c := range cands {
		abs, err := filepath.Abs(c)
		if err != nil {
			abs = c
		}
		key := strings.ToLower(abs) // Windows is case-insensitive
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("LookPath returned %s but no replaceable binary was found around it", p)
	}
	return out, nil
}

// replaceFile copies src → dst atomically (write to dst.new, rename
// over dst). Falls back to a direct copy on filesystems that don't
// allow cross-device renames.
//
// On Windows, a running process holding dst open (e.g. a live `crush
// run` session) makes a straight rename-over-dst fail: Windows refuses
// to replace a file that's in use. Deleting dst first doesn't help
// either — deleting a busy file is *also* refused there (confirmed by
// hand, see docs/plans/2026-07-29-relaunch-from-cache.md §2). What DOES
// work is renaming dst ASIDE (to "dst.old-<token>") rather than deleting
// it: Windows only cares about the open file's data, not its name, so
// the live process keeps running against the renamed file while dst
// becomes free for the new binary. The renamed-aside file lingers until
// a later deploy's SweepRenameAsideLeftovers can finally remove it, once
// every process still holding it has exited.
func replaceFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		if errors.Is(err, os.ErrExist) || runtime.GOOS == "windows" {
			if _, statErr := os.Stat(dst); statErr == nil {
				aside := deploy.RenameAsideName(dst, fmt.Sprintf("%d", time.Now().UnixNano()))
				if err2 := os.Rename(dst, aside); err2 != nil {
					return fmt.Errorf("rename-aside %s → %s: %w", dst, aside, err2)
				}
			}
			if err2 := os.Rename(tmp, dst); err2 != nil {
				return fmt.Errorf("rename %s → %s after rename-aside: %w", tmp, dst, err2)
			}
			return nil
		}
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
