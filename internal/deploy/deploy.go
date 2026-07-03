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
)

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
