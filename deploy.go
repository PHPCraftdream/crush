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

	// 2. Find destinations. CRUSH_DEPLOY_PATH wins (single target);
	//    otherwise we collect every crush binary on PATH that should be
	//    kept in sync — typically the npm-shim sibling AND the
	//    node_modules real binary, because Windows cmd's PATH resolution
	//    can pick either depending on the order of npm-dir vs other
	//    entries. Deploying to both stops "crush --version is fresh but
	//    crush claude-init says Unknown flag --replace" puzzles.
	dsts, err := resolveDests()
	if err != nil {
		fatal("could not determine deploy destination: %v\n  set CRUSH_DEPLOY_PATH=/full/path/to/crush(.exe) to force one", err)
	}
	for _, dst := range dsts {
		if sameFile(src, dst) {
			fatal("source and destination are the same file (%s) — nothing to replace", dst)
		}
	}
	step("Will replace %d path(s):", len(dsts))
	for _, dst := range dsts {
		fmt.Printf("    %s\n", dst)
	}

	// 3. Kill any running crush so the file is no longer locked (Windows)
	//    and the new binary takes effect on next launch (Unix).
	killAllCrush()

	// 4. Copy with a temp + rename to make the swap atomic for each
	//    target. If any single replace fails we bail — leaving a mixed
	//    state is worse than leaving the original.
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
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "crush.exe"
	}
	return "crush"
}

// resolveDests decides what files to overwrite. Priority:
//  1. $CRUSH_DEPLOY_PATH set → single forced target, used as-is.
//  2. Otherwise we discover the npm-installed crush via exec.LookPath
//     and return EVERY binary we can find around it:
//       a. <npm-dir>/node_modules/@charmland/crush/bin/crush(.exe)
//          — the real binary the JS wrapper execs via `node`.
//       b. <npm-dir>/crush.exe (Windows only) — a sibling native
//          binary that `cmd` may pick BEFORE the JS wrapper depending
//          on PATHEXT and PATH ordering. Historically this slot
//          received an out-of-band copy from a previous install and
//          then drifted, producing "crush --version is fresh but
//          claude-init --replace is unknown" symptoms.
//       c. The LookPath result itself, only if it is an executable
//          (.exe on Windows, no extension on Unix) — covers raw
//          binaries dropped on PATH without an npm package around
//          them.
// We deduplicate by absolute path so the same file isn't replaced
// twice on a same-file collision.
func resolveDests() ([]string, error) {
	if env := os.Getenv("CRUSH_DEPLOY_PATH"); env != "" {
		return []string{env}, nil
	}
	p, err := exec.LookPath("crush")
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(p)

	var cands []string
	// (a) node_modules real binary.
	npmBin := filepath.Join(dir, "node_modules", "@charmland", "crush", "bin", binaryName())
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
	if isReplaceableExe(p) {
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

// isReplaceableExe reports whether p is a native executable we should
// overwrite — not a .cmd/.ps1/POSIX shim. On Windows that means a .exe
// extension; on Unix it means an executable mode bit and no extension
// that screams "script".
func isReplaceableExe(p string) bool {
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
