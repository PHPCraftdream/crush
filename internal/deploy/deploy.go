// Package deploy holds the pure, testable decision logic behind
// deploy.go (the `go run deploy.go` install/upgrade script at the repo
// root). deploy.go itself carries `//go:build ignore` so it is never
// part of a normal `go build ./...`/`go test ./...` run and is invoked
// only via `go run`; splitting the logic that doesn't need to actually
// touch the registry, kill processes, or copy files into this package
// lets `go test ./...` exercise it — including on every OS in the CI
// build matrix (ubuntu-latest, macos-latest, windows-latest).
package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// RenameAsideName returns the path dst should be moved to before a fresh
// binary is written in its place, given a uniqueness token (normally
// time.Now().UnixNano(), passed in as a string so this stays a pure
// function over its inputs and is unit-testable without touching the
// clock). Windows will not let us delete a file backing a running
// process's image, but it will let us rename it out of the way and then
// write a new file under the freed name (confirmed by hand on 2026-07-29 —
// see docs/plans/2026-07-29-relaunch-from-cache.md §2): the running
// process keeps paging from the renamed file's data, and the name becomes
// free for the replacement. The renamed-aside file is swept up by
// SweepRenameAsideLeftovers on a later deploy, once the process holding it
// has exited and delete finally succeeds.
func RenameAsideName(dst, token string) string {
	return dst + ".old-" + token
}

// TempBuildName returns the temp path deploy.go's replaceFile writes the
// freshly built binary to before renaming it into dst's slot. The token
// (normally the same process-unique token used for the rename-aside name)
// makes the path unique per deploy, so two concurrent `go run deploy.go`
// invocations — or a deploy overlapping the npm wrapper's own copy — do
// not write to a single shared "dst.new" file and clobber each other's
// partial copy. Pure function over its inputs — unit-testable.
func TempBuildName(dst, token string) string {
	return dst + ".new-" + token
}

// SwapRenameAside performs the rename-aside swap deploy.go's replaceFile
// uses once a straight rename-over-dst has been refused (dst exists and,
// on Windows, is busy): move the existing dst out of the way to aside,
// then move the freshly written tmp into dst's slot. It is the second
// step — moving tmp into the now-freed dst — that this function guards.
//
// If moving tmp into dst fails (e.g. an antivirus transiently locks the
// freshly created tmp on Windows), dst would be left MISSING: the old
// binary was already moved to aside in the first step, and the new one
// never arrived. To avoid leaving the operator without a working binary,
// SwapRenameAside immediately attempts to move aside back to dst. The
// caller (deploy.go) passes os.Rename as rename; tests pass a fault-
// injecting rename to exercise the rollback deterministically on every
// OS, since making the real os.Rename fail at exactly this step is not
// reliably reproducible.
//
// Returns:
//   - nil on success: the new binary is at dst, the old one at aside.
//   - a non-nil error if the second rename failed but the old binary was
//     restored from aside to dst (dst is whole again — the failed deploy
//     left things exactly as they were; the operator can rerun).
//   - a composite error naming BOTH aside and dst if the restore-back
//     ALSO failed: dst is now missing and the old binary is stranded at
//     aside. This is the rare catastrophic case (two distinct transient
//     locks); the error names both paths so recovery is a single move.
func SwapRenameAside(rename func(old, new string) error, tmp, dst, aside string) error {
	if err := rename(dst, aside); err != nil {
		return fmt.Errorf("rename-aside %s → %s: %w", dst, aside, err)
	}
	if err := rename(tmp, dst); err != nil {
		// dst is now EMPTY — the old binary is stranded at aside. Try to
		// put it back so the operator is not left without a working crush.
		if rerr := rename(aside, dst); rerr != nil {
			return fmt.Errorf("rename %s → %s after rename-aside failed: %v; restore %s → %s ALSO failed — dst %s is now MISSING, recover manually by moving %s back to %s: %w", tmp, dst, err, aside, dst, dst, aside, dst, rerr)
		}
		// Restored: dst holds the old binary again. Surface the original
		// failure so the caller knows the deploy did not take.
		return fmt.Errorf("rename %s → %s after rename-aside failed (old binary restored to %s from %s): %w", tmp, dst, dst, aside, err)
	}
	return nil
}

// staleTempFileAge is how old a leftover ".new-<token>" temp file (see
// TempBuildName) must be before SweepRenameAsideLeftovers will delete it.
// These files are only ever created mid-copy by a deploy that is still
// running, so a fresh one may belong to a concurrent `go run deploy.go`
// (an accepted, documented race — see replaceFile's doc comment) and must
// not be swept out from under it. A file this old can only be a leftover
// from a deploy that crashed, was killed, or lost power before its own
// `defer os.Remove(tmp)` ran — no legitimate deploy takes anywhere near
// this long.
const staleTempFileAge = time.Hour

// SweepRenameAsideLeftovers best-effort deletes leftover files next to
// dst from previous deploys: the ".old-<token>" rename-aside files this
// package creates (via RenameAsideName), the legacy ".bak-*" files an
// older deploy mechanism left behind (observed on disk, not hypothetical
// — see the plan's grabli registry, Г11), and ".new-<token>" temp-copy
// files (via TempBuildName) old enough to be certain they were abandoned
// by a crashed deploy rather than belonging to one still in progress. It
// returns the paths it successfully removed; failures (most commonly a
// busy file still held open by a live crush process) are swallowed on
// purpose — a file that refuses to delete just means a live session is
// still serving it, and it will be swept on a later deploy once that
// session exits. This function never returns an error and never panics:
// a deploy must not fail just because old garbage from a previous run
// couldn't be cleaned up yet.
func SweepRenameAsideLeftovers(dst string) []string {
	// Enumerate dst's directory and match by prefix instead of using
	// filepath.Glob(dst + ".old-*") etc.: Glob treats '[', '?', and '*' in
	// the pattern as metacharacters, so an install path that happens to
	// contain any of those (Glob offers no escaping mechanism) would silently
	// match the wrong set of files — or none at all — instead of just the
	// leftovers next to dst. os.ReadDir + strings.HasPrefix compares dst's
	// basename literally, so it's correct regardless of what characters
	// appear in the path.
	dir := filepath.Dir(dst)
	base := filepath.Base(dst)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var removed []string
	for _, suffix := range []string{".old-", ".bak-"} {
		prefix := base + suffix
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
				continue
			}
			m := filepath.Join(dir, e.Name())
			if os.Remove(m) == nil {
				removed = append(removed, m)
			}
		}
	}

	newPrefix := base + ".new-"
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), newPrefix) {
			continue
		}
		m := filepath.Join(dir, e.Name())
		fi, statErr := os.Stat(m)
		if statErr != nil || time.Since(fi.ModTime()) < staleTempFileAge {
			continue
		}
		if os.Remove(m) == nil {
			removed = append(removed, m)
		}
	}
	return removed
}

// DefaultInstallPath returns the standard per-user install location for
// the running OS — reachable without admin/root rights:
//
//	Windows:      %LOCALAPPDATA%\Programs\crush\crush.exe
//	              (the canonical per-user programs dir, same convention
//	              as VS Code's user setup and winget user installs)
//	Linux/macOS:  ~/.local/bin/crush
//	              (XDG-recommended user binaries dir; on PATH by
//	              default in most modern distros and shells)
func DefaultInstallPath() (string, error) {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(base, "Programs", "crush", "crush.exe"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory for install location: %w", err)
	}
	return filepath.Join(home, ".local", "bin", "crush"), nil
}

// IsReplaceableExe reports whether p is a native executable that should
// be overwritten by a deploy — not a .cmd/.ps1/POSIX shim. On Windows
// that means a .exe extension; on Unix it means an executable mode bit
// and no extension that screams "script".
func IsReplaceableExe(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	if runtime.GOOS == "windows" {
		return ext == ".exe"
	}
	switch ext {
	case ".sh", ".bash", ".py", ".js", ".cjs", ".mjs":
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// SameFile reports whether a and b resolve to the same file on disk.
// Returns false (never errors) if either path can't be stat'd, since
// that's the safe answer for "would replacing b clobber a" checks.
func SameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// PathListContains reports whether dir is already present in pathEnv (a
// PATH-style string using the OS list separator). Comparison is
// case-insensitive on Windows (PATH is case-insensitive there) and
// case-sensitive elsewhere. Pure function — no filesystem or env
// access — so it's testable with a synthetic PATH string on any OS.
func PathListContains(pathEnv, dir string) bool {
	for _, entry := range filepath.SplitList(pathEnv) {
		entry = strings.TrimSpace(entry)
		if runtime.GOOS == "windows" {
			if strings.EqualFold(entry, dir) {
				return true
			}
			continue
		}
		if entry == dir {
			return true
		}
	}
	return false
}

// AppendToPathList appends dir to pathEnv using the OS list separator,
// unless it is already present (per PathListContains), in which case
// pathEnv is returned unchanged. Pure function, no side effects.
func AppendToPathList(pathEnv, dir string) string {
	if PathListContains(pathEnv, dir) {
		return pathEnv
	}
	if pathEnv == "" {
		return dir
	}
	return pathEnv + string(os.PathListSeparator) + dir
}

// LookPathExcludingCwd walks pathEnv (a PATH-style string) and returns
// the first `name` executable found in a directory that is NOT cwd.
// Mirrors exec.LookPath's semantics (uses extList — PATHEXT-style
// extensions on Windows, a single "" entry on Unix) but treats the cwd
// entry as invisible, matching deploy.go's need to ignore the freshly
// built local artifact when discovering an existing install elsewhere
// on PATH.
//
// cwd, pathEnv and extList are passed in explicitly (rather than read
// from os.Getwd/os.Getenv) so this is a pure function over its inputs
// and can be unit-tested with temp directories on any OS.
func LookPathExcludingCwd(name, cwd, pathEnv string, extList []string) (string, error) {
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolving cwd: %w", err)
	}

	skippedCwdHit := ""
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			// On Windows an empty PATH entry historically means cwd —
			// skip it for the same reason exec.LookPath does.
			continue
		}
		absDir, derr := filepath.Abs(dir)
		if derr == nil && strings.EqualFold(absDir, cwdAbs) {
			for _, ext := range extList {
				cand := filepath.Join(dir, name+ext)
				if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
					skippedCwdHit = cand
					break
				}
			}
			continue
		}
		for _, ext := range extList {
			cand := filepath.Join(dir, name+ext)
			fi, err := os.Stat(cand)
			if err != nil || fi.IsDir() {
				continue
			}
			if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
				continue
			}
			return cand, nil
		}
	}
	if skippedCwdHit != "" {
		return "", fmt.Errorf("only candidate found was %s (in current directory — refusing to treat the just-built artifact as an existing install). Install crush via npm/winget first, or set CRUSH_DEPLOY_PATH=/full/path/to/crush.exe", skippedCwdHit)
	}
	return "", fmt.Errorf("%s not found on PATH (excluding current directory)", name)
}

// WindowsPathExts returns the extension list LookPathExcludingCwd should
// try on Windows, derived from PATHEXT (falling back to the standard
// default set when PATHEXT is unset, matching cmd.exe's own behavior).
func WindowsPathExts(pathext string) []string {
	if pathext == "" {
		return []string{".exe", ".cmd", ".bat", ".com"}
	}
	var exts []string
	for _, e := range filepath.SplitList(pathext) {
		exts = append(exts, strings.ToLower(strings.TrimSpace(e)))
	}
	return exts
}

// NpmNodeOS maps a Go GOOS value to the Node "os" package name used for
// the fork's per-platform npm packages
// (node_modules/@phpcraftdream/crush-<node_os>-<node_arch>/bin/<binaryName>),
// mirroring the TARGETS table in .github/workflows/publish-fork-npm.yml.
// goos is passed in explicitly (rather than read from runtime.GOOS)
// so this stays a pure function over its input and every branch can be
// unit-tested from a single CI runner regardless of that runner's own OS.
func NpmNodeOS(goos string) string {
	if goos == "windows" {
		return "win32"
	}
	return goos // "linux", "darwin"
}

// NpmNodeArch maps a Go GOARCH value to the Node "cpu" package name used
// for the fork's per-platform npm packages. goarch is passed in
// explicitly (rather than read from runtime.GOARCH) for the same
// testability reason as NpmNodeOS.
func NpmNodeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		return goarch
	}
}

// NpmPlatformBinaryPath returns the path to the real binary inside the
// fork's per-platform npm package — the one the JS wrapper execs via
// `node` — given the npm package directory (the dir containing
// node_modules, i.e. the directory holding the `crush` entry found on
// PATH), the target OS/arch (as Go GOOS/GOARCH values), and the local
// binary name (crush or crush.exe). The fork ships the binary in the
// PLATFORM package, not the meta package (unlike upstream's
// @charmland/crush, which bundled bin/ directly in the package the JS
// wrapper lives in) — see docs/plans/2026-07-29-relaunch-from-cache.md
// §5.3. Pure function over its inputs — no filesystem access — so it's
// unit-testable for any OS/arch combination on any runner.
func NpmPlatformBinaryPath(npmDir, goos, goarch, binaryName string) string {
	return filepath.Join(npmDir, "node_modules", "@phpcraftdream",
		fmt.Sprintf("crush-%s-%s", NpmNodeOS(goos), NpmNodeArch(goarch)), "bin", binaryName)
}
