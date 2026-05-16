//go:build ignore

// deploy.go — one-shot: prod-build the binary, kill every running crush
// process on this machine, then replace whatever `crush` resolves to on
// PATH with the freshly built artifact. Verifies by running the new
// binary's --version.
//
// Usage:   go run deploy.go
// On Windows the kill uses taskkill /F /IM crush.exe; on Unix it uses
// pkill -f crush (graceful first, then -9 if anything is still around).
// Override the destination path with CRUSH_DEPLOY_PATH=...

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
)

func step(msg string, args ...any) { fmt.Printf("→ "+msg+"\n", args...) }
func ok(msg string, args ...any)   { fmt.Printf("✓ "+msg+"\n", args...) }
func warn(msg string, args ...any) { fmt.Printf("! "+msg+"\n", args...) }
func fatal(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "✗ "+msg+"\n", args...)
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
// surfaced as ok=false; we use this for best-effort kill calls where a
// "no such process" return code is not a failure.
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

	// 2. Find destination. CRUSH_DEPLOY_PATH wins; otherwise pick the
	//    crush on PATH so "what `which crush` returns" gets replaced.
	dst, err := resolveDest()
	if err != nil {
		fatal("could not determine deploy destination: %v\n  set CRUSH_DEPLOY_PATH=/full/path/to/crush(.exe) to force one", err)
	}
	if sameFile(src, dst) {
		fatal("source and destination are the same file (%s) — nothing to replace", dst)
	}
	step("Will replace: %s", dst)

	// 3. Kill any running crush so the file is no longer locked (Windows)
	//    and the new binary takes effect on next launch (Unix).
	killAllCrush()

	// 4. Copy with a temp + rename to make the swap atomic. If the dst
	//    is a shim script (npm wrapper case), back it up next to itself
	//    rather than overwriting — the .exe sibling is what we really
	//    want to update.
	if err := replaceFile(src, dst); err != nil {
		fatal("replace %s: %v", dst, err)
	}
	ok("Replaced %s", dst)

	// 5. Verify by running the newly installed binary.
	if v, err := exec.Command(dst, "--version").CombinedOutput(); err == nil {
		ok("Installed: %s", strings.TrimSpace(string(v)))
	} else {
		warn("--version probe failed (this may be fine if the dst is a non-exe shim): %v", err)
	}
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "crush.exe"
	}
	return "crush"
}

// resolveDest decides what file to overwrite. Priority:
//  1. $CRUSH_DEPLOY_PATH (used as-is).
//  2. The npm-packaged binary at
//     <npm-dir>/node_modules/@charmland/crush/bin/crush(.exe) — that's
//     the file the npm wrapper actually execs, and it's the case you
//     hit when `npm i -g @charmland/crush` put crush on your PATH.
//     (Replacing the sibling crush.exe of the wrapper does nothing —
//     the wrapper is a JS loader that points elsewhere.)
//  3. `exec.LookPath("crush")` — the wrapper itself, as a last resort
//     (covers the case where someone dropped a plain binary on PATH).
func resolveDest() (string, error) {
	if env := os.Getenv("CRUSH_DEPLOY_PATH"); env != "" {
		return env, nil
	}
	p, err := exec.LookPath("crush")
	if err != nil {
		return "", err
	}
	// If the discovered crush is a wrapper next to an npm package, the
	// real binary lives at node_modules/@charmland/crush/bin/crush(.exe).
	dir := filepath.Dir(p)
	npmBin := filepath.Join(dir, "node_modules", "@charmland", "crush", "bin", binaryName())
	if _, err := os.Stat(npmBin); err == nil {
		return npmBin, nil
	}
	// Otherwise, on Windows, prefer a sibling crush.exe if the wrapper
	// is a .cmd/.ps1/POSIX shim.
	if runtime.GOOS == "windows" {
		exe := filepath.Join(dir, "crush.exe")
		if _, err := os.Stat(exe); err == nil {
			return exe, nil
		}
	}
	return p, nil
}

func sameFile(a, b string) bool {
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

func killAllCrush() {
	step("Killing any running crush processes…")
	if runtime.GOOS == "windows" {
		// /F = force, /IM = image name. /T would also kill children;
		// we don't need that — we're after the parent process.
		if out, ok := runQuiet("taskkill", "/F", "/IM", "crush.exe"); ok {
			fmt.Print(out)
		} else {
			fmt.Print(out) // taskkill prints "ERROR: not found" — that's fine
		}
		// Best-effort second pass in case a copy of the dev binary is
		// running under a different filename (e.g. /tmp/crush.exe used
		// in smoke tests).
		runQuiet("taskkill", "/F", "/FI", "IMAGENAME eq crush*")
		return
	}
	// Unix: graceful first, then hard.
	runQuiet("pkill", "-x", "crush")
	time.Sleep(300 * time.Millisecond)
	runQuiet("pkill", "-9", "-x", "crush")
}

// replaceFile copies src → dst atomically (write to dst.new, rename
// over dst). Falls back to a direct copy on filesystems that don't
// allow cross-device renames.
func replaceFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Windows sometimes refuses to rename over a busy file even
		// after taskkill — fall back to delete + rename.
		if errors.Is(err, os.ErrExist) || runtime.GOOS == "windows" {
			_ = os.Remove(dst)
			if err2 := os.Rename(tmp, dst); err2 != nil {
				return fmt.Errorf("rename %s → %s after remove: %w", tmp, dst, err2)
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
